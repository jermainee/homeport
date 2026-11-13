package controlplane

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/jermainee/homeport/gateway"
)

const (
	// IdleTimeout ends a session after this long without a byte in either
	// direction. Its lease is returned then.
	IdleTimeout = 2 * time.Minute

	cutInterval = 30 * time.Second // a usage event at least this often...
	cutBytes    = 50_000_000       // ...or after this many bytes (50 MB)

	leaseTimeout = 5 * time.Second  // a lease request that takes longer counts as API unreachable
	leaseRetry   = 10 * time.Second // between lease requests without a lease to show for it
	renewLead    = time.Minute      // renew this long before the current lease expires
	cooldown     = time.Minute      // a closed session's peer is cut off this long
	defaultGrace = 5 * time.Minute  // top-up-only time before any lease said otherwise
)

type sessionState int

const (
	statePending sessionState = iota // waiting for the first lease answer
	stateActive                      // a lease with bytes left
	stateGrace                       // no usable lease: top-up only until graceUntil
)

// session is one peer's use of the gateway, from its first byte until it
// is idle for IdleTimeout, its grace runs out, the API says the peer is
// inactive, or the peer leaves the config.
type session struct {
	id          string
	state       sessionState
	decided     chan struct{} // closed once the first lease request is answered
	startedAt   time.Time
	exitIP      netip.Addr
	lineID      string
	seq         int
	periodStart time.Time

	lease     *Lease // the lease bytes count against
	leaseBase uint64 // total bytes reported when lease became current
	next      *Lease // granted successor, not used yet

	inFlight   bool      // a lease request is running
	requestID  string    // of the current logical lease request; kept for retries
	retryAt    time.Time // no new lease request before this
	denyGrace  int       // grace_seconds of the last 402, or -1
	graceUntil time.Time

	ended   bool
	finalID string // the final usage event, once the session ended with one
}

func total(u gateway.Usage) uint64 { return u.Up + u.Down }

// Access tells what a new flow of peer p may reach. The first flow of a
// session asks for its lease and waits for the answer, or until ctx ends.
func (c *Controller) Access(ctx context.Context, p gateway.Peer) gateway.Access {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	ps := c.peers[p.ID]
	if ps == nil || ps.peer.PublicKey != p.PublicKey || !ps.suspendedUntil.IsZero() {
		return gateway.AccessNone
	}
	if ps.sess == nil && !c.startSessionLocked(ps, now) {
		return gateway.AccessNone
	}
	ps.lastActive = now
	for {
		s := ps.sess
		if s == nil || c.peers[p.ID] != ps {
			return gateway.AccessNone
		}
		switch s.state {
		case stateActive:
			return gateway.AccessFull
		case stateGrace:
			return gateway.AccessTopUp
		}
		c.mu.Unlock()
		select {
		case <-s.decided:
		case <-ctx.Done():
		}
		c.mu.Lock()
		if ctx.Err() != nil {
			return gateway.AccessNone
		}
	}
}

// startSessionLocked starts a session for ps and asks for its lease. It
// does not when the peer's line is offline or is the member's own: no flow
// could pass then, and the bytes so far go into the next session.
func (c *Controller) startSessionLocked(ps *peerState, now time.Time) bool {
	line, ok := c.lines[ps.peer.LineID]
	if !ok || line.OwnerMemberID == ps.peer.MemberID {
		return false
	}
	c.lineMu.Lock()
	ip, online := c.lineIPs[ps.peer.LineID]
	c.lineMu.Unlock()
	if !online {
		return false
	}
	start := now.Truncate(time.Millisecond)
	s := &session{
		id:          newUUIDv7(now),
		decided:     make(chan struct{}),
		startedAt:   start,
		periodStart: start,
		exitIP:      ip,
		lineID:      ps.peer.LineID,
		denyGrace:   -1,
	}
	ps.sess = s
	ps.lastActive = now
	c.log.Info("session started", "peer_id", ps.cfg.PeerID, "session_id", s.id, "line_id", s.lineID)
	c.requestLeaseLocked(ps, s, now)
	return true
}

// Step does the controller's timed work: it starts sessions for peers that
// began sending, cuts usage events, renews and switches leases, ends grace
// periods and idle sessions, and lets closed peers back after the cooldown.
func (c *Controller) Step() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gw == nil {
		return
	}
	now := c.clock.Now()
	for _, ps := range c.peers {
		if !ps.suspendedUntil.IsZero() {
			if now.Before(ps.suspendedUntil) {
				continue
			}
			c.gw.SuspendPeer(ps.peer.PublicKey, false)
			ps.suspendedUntil = time.Time{}
		}
		u := c.usageLocked(ps)
		if u != ps.lastSeen {
			ps.lastSeen, ps.lastActive = u, now
			if ps.sess == nil {
				c.startSessionLocked(ps, now)
			}
		}
		if ps.sess != nil {
			c.stepSessionLocked(ps, ps.sess, u, now)
		}
	}
}

func (c *Controller) stepSessionLocked(ps *peerState, s *session, u gateway.Usage, now time.Time) {
	if s.state == stateGrace && !now.Before(s.graceUntil) {
		c.closeSessionLocked(ps, now, "grace over")
		return
	}
	if s.lease != nil {
		used := total(u) - s.leaseBase
		if s.state == stateActive && (used >= uint64(s.lease.GrantedBytes) || !now.Before(s.lease.ExpiresAt)) { // #nosec G115 -- granted_bytes is at least 1
			if s.next != nil {
				c.switchLeaseLocked(ps, s, u, now)
			} else {
				c.enterGraceLocked(ps, s, now)
			}
			used = total(u) - s.leaseBase
		}
		renew := s.state == stateGrace ||
			used >= uint64(s.lease.RenewAtBytes) || // #nosec G115 -- renew_at_bytes is at least 1
			!now.Before(s.lease.ExpiresAt.Add(-renewLead))
		if renew && s.next == nil && !s.inFlight && !now.Before(s.retryAt) {
			c.requestLeaseLocked(ps, s, now)
		}
		unreported := total(u) - total(ps.reported)
		if unreported >= cutBytes || (unreported > 0 && now.Sub(s.periodStart) >= cutInterval) {
			c.cutLocked(ps, s, u, now, false)
		}
	} else if s.state == stateGrace && !s.inFlight && !now.Before(s.retryAt) {
		c.requestLeaseLocked(ps, s, now)
	}
	if now.Sub(ps.lastActive) >= IdleTimeout {
		c.endSessionLocked(ps, u, now, "idle")
	}
}

// requestLeaseLocked asks for the session's next lease in the background.
// A retry of a request the API did not answer (unreachable, 429, 5xx) keeps
// its request_id; any other answer ends that logical request.
func (c *Controller) requestLeaseLocked(ps *peerState, s *session, now time.Time) {
	if s.requestID == "" {
		s.requestID = newUUIDv7(now)
	}
	req := LeaseRequest{RequestID: s.requestID, PeerID: ps.cfg.PeerID, SessionID: s.id}
	if s.lease != nil {
		req.SupersedesLeaseID = s.lease.LeaseID
	}
	s.inFlight = true
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Done()
		ctx, cancel := context.WithTimeout(context.Background(), leaseTimeout)
		lease, err := c.api.CreateLease(ctx, req)
		cancel()
		c.mu.Lock()
		defer c.mu.Unlock()
		c.leaseAnsweredLocked(ps, s, lease, err)
	}()
}

func (c *Controller) leaseAnsweredLocked(ps *peerState, s *session, lease Lease, err error) {
	s.inFlight = false
	now := c.clock.Now()
	if s.ended {
		if err == nil {
			c.returnLeaseLocked(s, lease.LeaseID, now) // a successor nobody will use
		}
		return
	}
	defer decide(s)
	var credits *InsufficientCreditsError
	switch {
	case err == nil:
		s.requestID, s.denyGrace = "", -1
		c.grace = time.Duration(lease.GraceSeconds) * time.Second
		switch {
		case s.lease == nil:
			s.lease, s.leaseBase, s.state = &lease, total(ps.reported), stateActive
		case lease.LeaseID != s.lease.LeaseID:
			s.next = &lease
			if s.state == stateGrace {
				c.switchLeaseLocked(ps, s, c.usageLocked(ps), now)
			}
		}
	case errors.Is(err, ErrPeerInactive):
		c.log.Info("peer inactive", "peer_id", ps.cfg.PeerID, "session_id", s.id)
		c.closeSessionLocked(ps, now, "peer inactive")
	case errors.As(err, &credits):
		s.requestID, s.denyGrace = "", credits.GraceSeconds
		s.retryAt = now.Add(leaseRetry)
		c.grace = time.Duration(credits.GraceSeconds) * time.Second
		if s.state == statePending {
			c.enterGraceLocked(ps, s, now)
		}
	default:
		c.log.Warn("lease request failed", "peer_id", ps.cfg.PeerID, "session_id", s.id, "err", err)
		if !Temporary(err) {
			s.requestID = ""
		}
		s.retryAt = now.Add(leaseRetry)
		if s.state == statePending {
			c.enterGraceLocked(ps, s, now)
		}
	}
}

func decide(s *session) {
	select {
	case <-s.decided:
	default:
		close(s.decided)
	}
}

// switchLeaseLocked closes the current lease's period with a usage event
// and makes the granted successor current.
func (c *Controller) switchLeaseLocked(ps *peerState, s *session, u gateway.Usage, now time.Time) {
	if total(u) > total(ps.reported) {
		c.cutLocked(ps, s, u, now, false)
	}
	s.lease, s.next = s.next, nil
	s.leaseBase, s.state = total(u), stateActive
}

// enterGraceLocked limits the session to the top-up hosts for the grace
// period of the last 402, else of the current lease, and resets the peer's
// open flows so nothing else keeps flowing.
func (c *Controller) enterGraceLocked(ps *peerState, s *session, now time.Time) {
	grace := c.grace
	switch {
	case s.denyGrace >= 0:
		grace = time.Duration(s.denyGrace) * time.Second
	case s.lease != nil:
		grace = time.Duration(s.lease.GraceSeconds) * time.Second
	}
	s.state, s.graceUntil = stateGrace, now.Add(grace)
	c.gw.ResetPeerFlows(ps.peer.PublicKey)
	c.log.Info("session top-up only", "peer_id", ps.cfg.PeerID, "session_id", s.id, "grace_seconds", int(grace.Seconds()))
}

// closeSessionLocked ends the session at once and cuts the peer off for the
// cooldown. The peer's packets stop being metered before the final count,
// so the final event holds every byte.
func (c *Controller) closeSessionLocked(ps *peerState, now time.Time, reason string) {
	c.gw.SuspendPeer(ps.peer.PublicKey, true)
	ps.suspendedUntil = now.Add(cooldown)
	c.endSessionLocked(ps, c.usageLocked(ps), now, reason)
}

// endSessionLocked reports the session's final usage u, and returns its
// leases once that final event is reported.
func (c *Controller) endSessionLocked(ps *peerState, u gateway.Usage, now time.Time, reason string) {
	s := ps.sess
	if s == nil {
		return
	}
	ps.sess = nil
	s.ended = true
	if s.lease != nil {
		s.finalID = c.cutLocked(ps, s, u, now, true)
		c.returnLeaseLocked(s, s.lease.LeaseID, now)
		if s.next != nil {
			c.returnLeaseLocked(s, s.next.LeaseID, now)
		}
	}
	decide(s)
	c.log.Info("session ended", "peer_id", ps.cfg.PeerID, "session_id", s.id, "reason", reason, "events", s.seq)
}

// cutLocked queues a usage event for the bytes since the last one and
// returns its ID. Periods are whole milliseconds and never empty.
func (c *Controller) cutLocked(ps *peerState, s *session, u gateway.Usage, now time.Time, final bool) string {
	end := now.Truncate(time.Millisecond)
	if !end.After(s.periodStart) {
		end = s.periodStart.Add(time.Millisecond)
	}
	ev := UsageEvent{
		EventID:     newUUIDv7(now),
		SessionID:   s.id,
		PeerID:      ps.cfg.PeerID,
		LineID:      s.lineID,
		LeaseID:     s.lease.LeaseID,
		Seq:         s.seq,
		BytesUp:     u.Up - ps.reported.Up,
		BytesDown:   u.Down - ps.reported.Down,
		PeriodStart: timestamp(s.periodStart),
		PeriodEnd:   timestamp(end),
		Final:       final,
	}
	if s.seq == 0 {
		ev.SessionStart = &SessionStart{StartedAt: timestamp(s.startedAt), ExitIP: s.exitIP.String()}
	}
	s.seq++
	s.periodStart = end
	ps.reported = u
	c.events = append(c.events, ev)
	return ev.EventID
}

// returnLeaseLocked queues a lease's return. A session's leases go back only
// after its final event is reported.
func (c *Controller) returnLeaseLocked(s *session, leaseID string, now time.Time) {
	r := leaseReturn{leaseID: leaseID, requestID: newUUIDv7(now)}
	if s.finalID != "" && c.awaitingFinal(s.finalID) {
		c.afterFinal[s.finalID] = append(c.afterFinal[s.finalID], r)
		return
	}
	c.returns = append(c.returns, r)
}

func (c *Controller) awaitingFinal(eventID string) bool {
	for _, e := range c.events {
		if e.EventID == eventID {
			return true
		}
	}
	return false
}

func (c *Controller) usageLocked(ps *peerState) gateway.Usage {
	return c.gw.PeerUsage(ps.peer.IPv4)
}
