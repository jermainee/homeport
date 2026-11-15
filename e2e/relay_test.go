//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jermainee/homeport/agent"
	"github.com/jermainee/homeport/e2e/internal/fakeapi"
)

const mb = 1_000_000 // AGENTS.md: 1 MB = 10^6 bytes

func TestTransfersArriveIntactAndEveryByteIsReportedOnce(t *testing.T) {
	s := newStack(t)

	for _, size := range []int{1 * mb, 10 * mb} {
		if got := s.get("/blob/" + itoa(size)); sum(got) != sum(blob(size)) {
			t.Fatalf("%d-byte download arrived as %d bytes with another hash", size, len(got))
		}
	}
	up := blob(1 * mb)
	if got := s.upload(up); got != sum(up) {
		t.Fatalf("the origin received an upload with hash %s, want %s", got, sum(up))
	}

	// The gateway meters exactly what the client's tun counted.
	c := s.settled(true)

	// The agent counts payload only: the transfers plus HTTP headers.
	st := s.agent.Stats()
	const headers = 4096 // generous room for 3 requests' and answers' headers
	if st.BytesIn < 11*mb || st.BytesIn > 11*mb+headers {
		t.Errorf("agent received %d bytes from the origin, want 11 MB plus headers", st.BytesIn)
	}
	if st.BytesOut < 1*mb || st.BytesOut > 1*mb+headers {
		t.Errorf("agent sent %d bytes to the origin, want 1 MB plus headers", st.BytesOut)
	}
	if st.FlowsTotal != 3 { // settled waited until none is open
		t.Errorf("agent saw %d flows, want 3", st.FlowsTotal)
	}
	// The gateway counts whole IP packets: the payload plus at most 10%
	// for IP and TCP headers, and the ACKs for the other direction.
	if d := c.gateway.Down; d < st.BytesIn || d > st.BytesIn*11/10+st.BytesOut/10 {
		t.Errorf("gateway metered %d bytes down for %d bytes of payload", d, st.BytesIn)
	}
	if u := c.gateway.Up; u < st.BytesOut || u > st.BytesOut*11/10+st.BytesIn/10 {
		t.Errorf("gateway metered %d bytes up for %d bytes of payload", u, st.BytesOut)
	}

	t.Logf("gateway %+v, client sent %d received %d, agent out %d in %d", c.gateway, c.sent, c.received, st.BytesOut, st.BytesIn)

	s.stopGateway()
	up2, down := s.api.UsageTotal()
	if up2 != c.gateway.Up || down != c.gateway.Down {
		t.Errorf("usage events report %d up and %d down; the gateway metered %+v", up2, down, c.gateway)
	}
	assertOneClosedSession(t, s.api)
}

// assertOneClosedSession checks that the API saw exactly one session, that
// it ended with a final event, and that its lease came back after that.
func assertOneClosedSession(t *testing.T, api *fakeapi.API) {
	t.Helper()
	events := api.Usage()
	if len(events) == 0 {
		t.Fatal("no usage events")
	}
	last := events[len(events)-1]
	for _, e := range events {
		if e.SessionID != last.SessionID {
			t.Fatalf("events of two sessions: %s and %s", e.SessionID, last.SessionID)
		}
	}
	if !last.Final {
		t.Errorf("the session's last event is not final")
	}
	finalSeq, returnSeq := -1, -1
	for _, c := range api.Calls() {
		switch {
		case c.Path == fakeapi.PathUsage && c.Final && c.Status == 200 && finalSeq < 0:
			finalSeq = c.Seq
		case strings.HasSuffix(c.Path, "/return") && c.Status == 200 && returnSeq < 0:
			returnSeq = c.Seq
		}
	}
	if returns := api.Returns(); len(returns) != 1 || returns[0] != last.LeaseID {
		t.Errorf("returned leases %v, want the session's lease %s", returns, last.LeaseID)
	}
	if finalSeq < 0 || returnSeq < finalSeq {
		t.Errorf("the lease return (call %d) did not follow the final usage event (call %d)", returnSeq, finalSeq)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestUDPAndDNSFlowsPassThroughTheAgent(t *testing.T) {
	s := newStack(t)

	conn, err := s.client.Net.DialUDPAddrPort(netip.AddrPort{}, udpDst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	buf := make([]byte, 2048)
	for _, payload := range [][]byte{[]byte("ping"), blob(1200)} {
		// Datagrams may get lost, so ask again until the echo comes back.
		// Under the race detector the first flow's setup (handshake, lease,
		// agent) can take seconds, longer than one wait, so the echo of an
		// earlier datagram may still arrive: only the payload's own counts.
		deadline := time.Now().Add(30 * time.Second)
		echoed := false
		for !echoed && time.Now().Before(deadline) {
			if _, err := conn.Write(payload); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(earliest(time.Now().Add(3*time.Second), deadline))
			for !echoed {
				n, err := conn.Read(buf)
				if err != nil {
					break // ask again
				}
				echoed = bytes.Equal(buf[:n], payload)
			}
		}
		if !echoed {
			t.Fatalf("no UDP echo of %d bytes came back", len(payload))
		}
	}

	var addrs []string
	err = untilDNSAnswers(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), waitFor)
		defer cancel()
		var err error
		addrs, err = s.client.Net.LookupContextHost(ctx, originName)
		return err
	})
	if err != nil {
		t.Fatalf("look up %s through the tunnel: %v", originName, err)
	}
	if !slices.Equal(addrs, []string{tcpDst.Addr().String()}) {
		t.Errorf("%s resolved to %v, want %s", originName, addrs, tcpDst.Addr())
	}
	if !s.resolver.looked("ip4 " + originName + ".") {
		t.Errorf("the agent's resolver was not asked for %s", originName)
	}
	var body []byte
	err = untilDNSAnswers(func() error {
		var err error
		body, err = s.tryGet("http://"+originName+contentPath+"/blob/5000", transferTimeout)
		return err
	})
	if err != nil || sum(body) != sum(blob(5000)) {
		t.Fatalf("fetch by name: %d bytes, %v", len(body), err)
	}

	// The gateway meters exactly what the client's tun counted. The UDP
	// and DNS flows stay open until they idle out.
	c := s.settled(false)
	s.stopGateway()
	if up, down := s.api.UsageTotal(); up != c.gateway.Up || down != c.gateway.Down {
		t.Errorf("usage events report %d up and %d down; the gateway metered %+v", up, down, c.gateway)
	}
}

func TestHomeNetworksAndMailPortsAreRefusedQuickly(t *testing.T) {
	s := newStack(t)
	s.get("/blob/1000") // the handshake, the session and its lease are done now

	for _, dst := range []string{"10.0.0.1:80", "192.168.1.1:80", "10.77.0.1:80", netip.AddrPortFrom(tcpDst.Addr(), 25).String()} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start := time.Now()
		conn, err := s.client.Net.DialContext(ctx, "tcp", dst)
		elapsed, timedOut := time.Since(start), ctx.Err() != nil
		cancel()
		switch {
		case err == nil:
			_ = conn.Close()
			t.Errorf("a connection to %s was opened", dst)
		case timedOut:
			t.Errorf("a connection to %s timed out instead of being reset", dst)
		case elapsed > 5*time.Second:
			t.Errorf("a connection to %s was refused after %v", dst, elapsed)
		}
	}
	// The gateway refused the home networks and its own tunnel address
	// before the agent saw them. The agent refused the mail port by its
	// policy, never dialing it. When the refusal takes longer than the
	// client's SYN timeout (1 s, as under the race detector), a
	// retransmitted SYN can reach the gateway after its RST and is refused
	// as a flow of its own, so a destination may be refused more than once.
	want := map[string]int{"blocked_port": 1}
	var got map[string]int
	waitUntil(t, "the agent logged its refusals", func() bool {
		got = map[string]int{}
		for _, rec := range logRecords(t, s.agentLogs) {
			if rec["msg"] == "flow refused by policy" {
				got[fmt.Sprint(rec["rule"])]++
			}
		}
		for rule, n := range want {
			if got[rule] < n {
				return false
			}
		}
		return true
	})
	if !maps.EqualFunc(got, want, func(int, int) bool { return true }) {
		t.Errorf("the agent refused by the rules %v, want only %v", got, want)
	}
	if st := s.agent.Stats(); st.FlowsTotal != 1 {
		t.Errorf("the agent opened %d flows, want only the first fetch", st.FlowsTotal)
	}
}

func TestTheSessionStopsAtTheLeaseAndClosesAfterGrace(t *testing.T) {
	// The gateway checks its leases once a second, so the download stays
	// well below (granted - renewAt) per second: the renewal is asked for
	// before the lease runs out, as on a line slower than the lease's
	// renewal margin. The lease and the 402 give the same grace, as the
	// API does.
	const (
		granted = 300_000
		renewAt = 100_000
		grace   = 5 // seconds of top-up-only access once the lease is used up
		rate    = 100_000
	)
	s := newStack(t, withLeases(func(n int, _ fakeapi.LeaseRequest) fakeapi.LeaseAnswer {
		if n == 0 {
			return fakeapi.Grant(granted, renewAt, grace)
		}
		return fakeapi.Deny(grace)
	}))

	// A slow download that would need far more than the lease.
	body, err := s.tryGet(fmt.Sprintf("http://%s%s/slow/%d/%d", tcpDst, contentPath, 20*mb, rate), transferTimeout)
	if err == nil {
		t.Fatalf("the download finished (%d bytes) on a %d-byte lease", len(body), granted)
	}

	// Grace: top-up only. DNS still works; anything else is refused.
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	addrs, dnsErr := s.client.Net.LookupContextHost(ctx, originName)
	cancel()
	if dnsErr != nil || len(addrs) == 0 {
		t.Errorf("DNS during grace: %v, %v", addrs, dnsErr)
	}
	if _, err := s.tryGet("http://"+tcpDst.String()+contentPath+"/blob/100", 5*time.Second); err == nil {
		t.Error("a new download worked during grace")
	}

	// After grace the session closes with a final usage event.
	waitUntil(t, "the session's final usage event arrived", func() bool {
		for _, c := range s.api.Calls() {
			if c.Path == fakeapi.PathUsage && c.Final && c.Status == 200 {
				return true
			}
		}
		return false
	})
	// The gateway's clock: grace starts when the lease runs out, lasts
	// grace_seconds and ends at the next one-second step after grace_until.
	// Each line is logged just after its clock read, so the top-up line
	// comes just after grace's start (grace_until less the grace) and the
	// close comes at or after grace_until.
	var toppedUpAt, graceUntil, closedAt time.Time
	graceSeconds := -1.0
	for _, rec := range logRecords(t, s.gwLogs) {
		at, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(rec["time"]))
		switch {
		case rec["msg"] == "session top-up only":
			toppedUpAt = at
			graceUntil, _ = time.Parse(time.RFC3339Nano, fmt.Sprint(rec["grace_until"]))
			graceSeconds, _ = rec["grace_seconds"].(float64)
		case rec["msg"] == "session ended" && rec["reason"] == "grace over":
			closedAt = at
		}
	}
	switch graceStart := graceUntil.Add(-grace * time.Second); {
	case graceUntil.IsZero() || graceSeconds != grace:
		t.Errorf("the top-up line gives grace_until %v and grace_seconds %v, want a time and %d", graceUntil, graceSeconds, grace)
	case closedAt.IsZero():
		t.Error(`no session ended with reason "grace over"`)
	default:
		if d := toppedUpAt.Sub(graceStart); d < 0 || d > time.Second {
			t.Errorf("the top-up line came %v after grace began (%v), want just after", d, graceStart)
		}
		if d := closedAt.Sub(graceUntil); d < 0 || d > 2*time.Second {
			t.Errorf("the session closed %v after grace_until (%v, closed at %v), want within a step and a second", d, graceUntil, closedAt)
		}
	}

	// The session is closed: the peer is cut off, and nothing more is metered.
	closed := s.meter()
	if _, err := s.tryGet("http://"+tcpDst.String()+contentPath+"/blob/100", 2*time.Second); err == nil {
		t.Error("a download worked after the session closed")
	}
	if m := s.meter(); m != closed {
		t.Errorf("the gateway metered %+v after the session closed, %+v before", m, closed)
	}
	// The lease is checked once a second; allow two seconds of traffic.
	if total := closed.Up + closed.Down; total < granted || total > granted+2*rate {
		t.Errorf("the session used %d bytes on a %d-byte lease; want at most %d bytes over", total, granted, 2*rate)
	}
	if up, down := s.api.UsageTotal(); up != closed.Up || down != closed.Down {
		t.Errorf("usage events report %d up and %d down; the gateway metered %+v", up, down, closed)
	}
	// The first lease, then renewals of it: one at renew_at_bytes, and
	// another every 10 s after a 402 while bytes are left.
	reqs := s.api.LeaseRequests()
	if events := s.api.Usage(); len(events) > 0 {
		lease := events[0].LeaseID
		if len(reqs) < 2 || reqs[0].SupersedesLeaseID != "" {
			t.Errorf("lease requests %+v, want the first lease and a renewal", reqs)
		}
		for _, r := range reqs[1:] {
			if r.SupersedesLeaseID != lease {
				t.Errorf("lease request %+v does not renew the session's lease %s", r, lease)
			}
		}
	}
	waitUntil(t, "the lease came back", func() bool { return len(s.api.Returns()) == 1 })
	assertOneClosedSession(t, s.api)
}

func TestARetriedUsageReportIsCountedOnce(t *testing.T) {
	for name, failure := range map[string]fakeapi.Failure{
		"500 before applying":        fakeapi.FailWith500,
		"answer lost after applying": fakeapi.ApplyThenDrop,
	} {
		t.Run(name, func(t *testing.T) {
			// Small leases, so the session switches leases mid-transfer
			// and the loop reports a usage event while traffic flows. The
			// download moves well under (granted - renew_at) per second,
			// the gateway's lease step.
			s := newStack(t, withLeases(func(int, fakeapi.LeaseRequest) fakeapi.LeaseAnswer {
				return fakeapi.Grant(1*mb, 200_000, 60)
			}))
			s.api.FailNext(fakeapi.PathUsage, failure)

			const size, rate = 1_200_000, 300_000
			body := s.get(fmt.Sprintf("/slow/%d/%d", size, rate))
			if sum(body) != sum(blob(size)) {
				t.Fatalf("download arrived as %d bytes with another hash", len(body))
			}
			waitUntil(t, "the failed usage report was sent again", func() bool {
				calls := usageCalls(s.api)
				return len(calls) >= 2 && calls[len(calls)-1].Status == 200
			})
			c := s.settled(true) // the gateway meters exactly what the client's tun counted
			s.stopGateway()

			calls := usageCalls(s.api)
			failed := calls[0]
			if failed.Status == 200 {
				t.Fatalf("the first usage report was answered %d", failed.Status)
			}
			// The retry sends the failed events first, with their IDs; events
			// cut since may ride along.
			retried := calls[1]
			if retried.Status != 200 || len(retried.EventIDs) < len(failed.EventIDs) || !slices.Equal(retried.EventIDs[:len(failed.EventIDs)], failed.EventIDs) {
				t.Errorf("the retry carried %v (answered %d); the failed report carried %v", retried.EventIDs, retried.Status, failed.EventIDs)
			}
			if up, down := s.api.UsageTotal(); up != c.gateway.Up || down != c.gateway.Down {
				t.Errorf("usage events report %d up and %d down; the gateway metered %+v", up, down, c.gateway)
			}
		})
	}
}

func usageCalls(api *fakeapi.API) []fakeapi.Call {
	var out []fakeapi.Call
	for _, c := range api.Calls() {
		if c.Path == fakeapi.PathUsage {
			out = append(out, c)
		}
	}
	return out
}

func TestTheAgentReconnectsAfterTheGatewayDropsIt(t *testing.T) {
	s := newStack(t)
	if got := s.get("/blob/100000"); sum(got) != sum(blob(100_000)) {
		t.Fatal("download before the restart differs")
	}
	first := s.settled(true).gateway

	// The gateway restarts on the same ports with the same keys, as after
	// a deploy. Its agents are dropped with it.
	s.stopGateway()
	waitUntil(t, "the agent noticed the gateway left", func() bool {
		return s.agent.Stats().State != agent.StateConnected
	})
	s.startGateway()
	s.waitLineOnline()
	// The member's WireGuard app reconnects too; a fresh client
	// handshakes at once instead of after WireGuard's 15 s timeout.
	s.newClient()
	if got := s.get("/blob/100000"); sum(got) != sum(blob(100_000)) {
		t.Fatal("download after the restart differs")
	}
	second := s.settled(true).gateway
	s.stopGateway()

	var types []string
	for _, e := range s.api.LineEvents() {
		types = append(types, e.Type)
	}
	if want := []string{"online", "offline", "online", "offline"}; !slices.Equal(types, want) {
		t.Errorf("line events %v, want %v", types, want)
	}
	if up, down := s.api.UsageTotal(); up != first.Up+second.Up || down != first.Down+second.Down {
		t.Errorf("usage events report %d up and %d down; the two gateways metered %+v and %+v", up, down, first, second)
	}
}

func TestShareTurnedOnWhileTheGatewayRunsWorksWithinSeconds(t *testing.T) {
	s := newStack(t, withShareLater())
	took := s.share()
	// The gateway polls its config every 30 s. The agent's first attempt is
	// refused, which makes the gateway fetch its config at once, and the
	// agent retries within a second.
	t.Logf("the line went online %v after Share was turned on", took.Round(time.Millisecond))
	if took > 3*time.Second {
		t.Errorf("the line went online %v after the member turned Share on; want within a few seconds", took)
	}
	// The same fetch brought member B's peer.
	if got := s.get("/blob/100000"); sum(got) != sum(blob(100_000)) {
		t.Fatal("download through the new line differs")
	}
}

func TestANewWireGuardPeerWorksWithinSeconds(t *testing.T) {
	s := newStack(t, withPeerLater())
	start := time.Now()
	s.addPeer()
	// The gateway polls its config every 30 s. It drops the client's first
	// handshake, from a key it does not know, and fetches its config at
	// once; the client sends its handshake again 5 s later, and that one
	// gets in.
	got := s.get("/blob/100000")
	took := time.Since(start)
	if sum(got) != sum(blob(100_000)) {
		t.Fatal("download through the new peer differs")
	}
	handshake, err := s.client.LastHandshake()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the new peer's handshake completed %v and its first download %v after its client started",
		handshake.Sub(start).Round(time.Millisecond), took.Round(time.Millisecond))
	if handshake.Sub(start) > 8*time.Second {
		t.Errorf("the new peer's handshake completed %v after its client started; want it at the client's first retry, within a few seconds", handshake.Sub(start))
	}
}

func TestLogsNeverNameADestination(t *testing.T) {
	s := newStack(t)
	// Every kind of flow: TCP, UDP, DNS, and refused ones.
	s.get("/blob/1000")
	if conn, err := s.client.Net.DialUDPAddrPort(netip.AddrPort{}, udpDst); err == nil {
		_, _ = conn.Write([]byte("ping"))
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _ = conn.Read(make([]byte, 16))
		_ = conn.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	_, _ = s.client.Net.LookupContextHost(ctx, originName)
	if conn, err := s.client.Net.DialContext(ctx, "tcp", "192.168.1.1:80"); err == nil {
		_ = conn.Close()
	}
	cancel()
	s.stopGateway()

	// The logs are not empty: they hold the session metadata.
	for name, logs := range map[string]*syncBuffer{"gateway": s.gwLogs, "agent": s.agentLogs} {
		if !strings.Contains(logs.String(), lineID) {
			t.Errorf("the %s's log does not name the line", name)
		}
	}
	if !strings.Contains(s.gwLogs.String(), peerID) {
		t.Error("the gateway's log does not name the peer")
	}
	s.assertLogsNameNoDestination()
}

// logRecords parses JSON log lines.
func logRecords(t *testing.T, b *syncBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err == nil {
			out = append(out, rec)
		}
	}
	return out
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// untilDNSAnswers runs f again while it fails with a DNS timeout, starting
// no new try after slowTimeout. The client's resolver (wireguard-go's
// netstack) gives a lookup two tries of 5 s each, and under the race
// detector a lookup through the tunnel can take longer than that; without
// it, a lookup takes about a millisecond.
func untilDNSAnswers(f func() error) error {
	deadline := time.Now().Add(slowTimeout)
	for {
		err := f()
		var dnsErr *net.DNSError
		if !errors.As(err, &dnsErr) || !dnsErr.IsTimeout || time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestAnHTTPSOnlyDeviceGetsNoPlainHTTPButStillResolves(t *testing.T) {
	s := newStack(t, withHTTPSOnly())

	if _, err := s.tryGet("http://"+tcpDst.String()+contentPath+"/blob/100", 5*time.Second); err == nil {
		t.Error("plain HTTP from a device with HTTPS only was answered")
	}
	err := untilDNSAnswers(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), waitFor)
		defer cancel()
		_, err := s.client.Net.LookupContextHost(ctx, originName)
		return err
	})
	if err != nil {
		t.Errorf("DNS through the virtual resolver: %v", err)
	}
	if n := s.agent.Stats().FlowsTotal; n != 1 {
		t.Errorf("the agent opened %d flows, want only the DNS flow: the plain TCP flow must not reach it", n)
	}
}
