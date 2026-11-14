package controlplane

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/jermainee/homeport/gateway"
)

const maxBatch = 100 // events per usage or line-event request

// semver is the contract's Version pattern.
var semver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// OnLineEvent records that a line's agent connected or disconnected, and
// queues the event for the API. It never blocks, so the gateway may call it
// from anywhere.
func (c *Controller) OnLineEvent(e gateway.LineEvent) {
	now := c.clock.Now()
	ev := LineEvent{
		EventID:      newUUIDv7(now),
		LineID:       e.LineID,
		Type:         "offline",
		ObservedIP:   e.ObservedIP.String(),
		AgentVersion: e.AgentVersion,
		At:           timestamp(now),
	}
	if e.Online {
		ev.Type = "online"
	}
	if !semver.MatchString(ev.AgentVersion) {
		// The API refuses a whole batch with a malformed event.
		ev.AgentVersion = "0.0.0"
	}
	c.lineMu.Lock()
	defer c.lineMu.Unlock()
	if e.Online {
		c.lineIPs[e.LineID] = e.ObservedIP
	} else {
		delete(c.lineIPs, e.LineID)
	}
	if e.ObservedIP.IsValid() {
		c.lineEvents = append(c.lineEvents, ev)
	}
}

// Settle waits for the lease requests that Step and Access started.
func (c *Controller) Settle() { c.inflight.Wait() }

// Flush reports queued usage events, then returns the leases whose final
// events are reported, then reports line events. What the API does not
// answer stays queued for the next Flush, with the same IDs.
func (c *Controller) Flush(ctx context.Context) {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	c.flushUsage(ctx)
	c.flushReturns(ctx)
	c.flushLineEvents(ctx)
}

func (c *Controller) flushUsage(ctx context.Context) {
	for {
		c.mu.Lock()
		batch := append([]UsageEvent(nil), c.events[:min(len(c.events), maxBatch)]...)
		c.mu.Unlock()
		if len(batch) == 0 {
			return
		}
		results, err := c.api.ReportUsage(ctx, batch)
		if err != nil && keepForRetry(err) {
			c.log.Warn("usage report failed; will retry", "events", len(batch), "err", err)
			return
		}
		c.mu.Lock()
		if err != nil {
			c.log.Error("usage report refused; dropping the batch", "events", len(batch), "err", err)
		}
		for i, ev := range batch {
			if err == nil && results[i].Status == "rejected" {
				c.log.Error("usage event rejected", "event_id", ev.EventID, "session_id", ev.SessionID, "peer_id", ev.PeerID, "reason", results[i].Reason)
			}
			c.returns = append(c.returns, c.afterFinal[ev.EventID]...)
			delete(c.afterFinal, ev.EventID)
		}
		// Only Flush removes events, and it holds flushMu, so the batch is
		// still at the head of the queue.
		c.events = c.events[len(batch):]
		c.mu.Unlock()
	}
}

func (c *Controller) flushReturns(ctx context.Context) {
	c.mu.Lock()
	pending := append([]leaseReturn(nil), c.returns...)
	c.mu.Unlock()
	done := 0
	for _, r := range pending {
		if err := c.api.ReturnLease(ctx, r.leaseID, r.requestID); err != nil {
			if keepForRetry(err) {
				c.log.Warn("lease return failed; will retry", "lease_id", r.leaseID, "err", err)
				break
			}
			c.log.Error("lease return refused", "lease_id", r.leaseID, "err", err)
		}
		done++
	}
	c.mu.Lock()
	c.returns = c.returns[done:]
	c.mu.Unlock()
}

func (c *Controller) flushLineEvents(ctx context.Context) {
	for {
		c.lineMu.Lock()
		batch := append([]LineEvent(nil), c.lineEvents[:min(len(c.lineEvents), maxBatch)]...)
		c.lineMu.Unlock()
		if len(batch) == 0 {
			return
		}
		results, err := c.api.ReportLineEvents(ctx, batch)
		if err != nil && keepForRetry(err) {
			c.log.Warn("line event report failed; will retry", "events", len(batch), "err", err)
			return
		}
		if err != nil {
			c.log.Error("line event report refused; dropping the batch", "events", len(batch), "err", err)
		}
		for i, ev := range batch {
			if err == nil && results[i].Status == "rejected" {
				c.log.Error("line event rejected", "event_id", ev.EventID, "line_id", ev.LineID, "reason", results[i].Reason)
			}
		}
		c.lineMu.Lock()
		c.lineEvents = c.lineEvents[len(batch):]
		c.lineMu.Unlock()
	}
}

// keepForRetry reports whether a failed report or return should be sent
// again later. Only answers that say the request itself is wrong (other
// 4xx) drop it; retrying those could never succeed.
func keepForRetry(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return true // for example a malformed answer
	}
	return Temporary(err) || ae.Status < 400 || ae.Status == http.StatusRequestTimeout
}

// Run polls the config every ConfigInterval and whenever RefreshConfig
// asks, steps every second and reports what is queued, until ctx ends. Call
// Close afterwards.
func (c *Controller) Run(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.syncLoop(ctx)
	}()
	go c.loop(ctx, time.Second, func() { c.Flush(ctx) })
	c.loop(ctx, time.Second, c.Step)
	<-done
}

// syncLoop polls the config now and every ConfigInterval, and fetches it
// when RefreshConfig asks, but at most once per ConfigRefreshInterval for
// RefreshConfig: an unknown key costs a handshake, so a flood of them must
// not turn into a flood of requests to the API.
func (c *Controller) syncLoop(ctx context.Context) {
	poll := time.NewTicker(ConfigInterval)
	defer poll.Stop()
	fetch := c.SyncConfig   // what the next fetch is: a poll or a refresh
	var refreshed time.Time // when the last refresh started
	for {
		// A refresh asked for before this fetch starts is answered by it.
		select {
		case <-c.refresh:
		default:
		}
		if err := fetch(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("config sync failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			fetch = c.SyncConfig
		case <-c.refresh:
			wait := time.NewTimer(time.Until(refreshed.Add(ConfigRefreshInterval)))
			select {
			case <-ctx.Done():
				wait.Stop()
				return
			case <-wait.C:
			}
			refreshed = time.Now()
			fetch = c.syncRefresh
		}
	}
}

// loop runs fn now and then every interval until ctx ends.
func (c *Controller) loop(ctx context.Context, interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		fn()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Close ends every session with a final usage event, then reports the
// events and returns the leases, as far as ctx allows. From then on no
// session starts: every flow is refused. Line events that come later, such
// as the gateway's agents going offline when it closes, are reported by a
// later Flush.
func (c *Controller) Close(ctx context.Context) {
	c.mu.Lock()
	c.closing = true
	now := c.clock.Now()
	for _, ps := range c.peers {
		if ps.sess != nil {
			c.endSessionLocked(ps, c.usageLocked(ps), now, "gateway stopping")
		}
	}
	c.mu.Unlock()
	c.Settle()
	c.Flush(ctx)
}
