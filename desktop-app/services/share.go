package services

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/jermainee/homeport/agent"
	relayprotocol "github.com/jermainee/homeport/relay-protocol"
	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/jermainee/homeport/desktop-app/internal/apiclient"
	"github.com/jermainee/homeport/desktop-app/internal/profile"
)

// ShareState is what Share is doing, as the Share screen shows it.
type ShareState string

const (
	// ShareOff: the member turned Share off.
	ShareOff ShareState = "off"
	// ShareConnecting: Share is on, and the line is being set up or the
	// agent is dialing the gateway.
	ShareConnecting ShareState = "connecting"
	// ShareOnline: the gateway accepted the agent; members can use the line.
	ShareOnline ShareState = "online"
	// SharePaused: another VPN carries this computer's default route, so the
	// agent stays away until it is gone.
	SharePaused ShareState = "paused"
	// ShareOffline: Share is on, but the API or the gateway can't be reached
	// (Error says which); it keeps trying.
	ShareOffline ShareState = "offline"
	// ShareError: Share turned itself off because of Error, which trying
	// again won't fix by itself.
	ShareError ShareState = "error"
)

// ShareStatus is Share's state and the agent's counters, as the Share
// screen and the tray show them.
type ShareStatus struct {
	State ShareState `json:"state"`
	// Sharing is the switch: whether the member wants Share on.
	Sharing bool `json:"sharing"`
	// Error says why Share is offline or stopped.
	Error ErrorCode `json:"error,omitempty"`
	// BytesToday is the data this computer relayed today, local time, as
	// the agent counts it: payload to and from destinations. The gateway's
	// count, which credits follow, includes packet headers too.
	BytesToday uint64 `json:"bytesToday"`
	// FlowsActive is the number of connections relayed right now.
	FlowsActive uint64 `json:"flowsActive"`
}

// Agent is the sharing agent as ShareService drives it. *agent.Agent is one;
// tests use a fake.
type Agent interface {
	// Run shares the line until ctx ends.
	Run(ctx context.Context) error
	// Stats reports the agent's state and counters.
	Stats() agent.Stats
}

// ShareOptions are ShareService's seams and settings. Zero values mean the
// defaults.
type ShareOptions struct {
	// NewAgent creates the agent. Nil means agent.New.
	NewAgent func(agent.Config) (Agent, error)
	// DeviceName is how this computer appears in the member's devices. Empty
	// means the host name.
	DeviceName string
	// Now is the clock that decides what "today" is. Nil means time.Now.
	Now func() time.Time
	// RetryMin and RetryMax bound the wait between attempts to set up the
	// line while the API can't be reached: 1 s doubling to 1 min.
	RetryMin, RetryMax time.Duration
	// StatusInterval is how often a running Share publishes its counters.
	// Zero means one second.
	StatusInterval time.Duration
	// Logger receives session metadata only. Nil discards logs.
	Logger *slog.Logger
}

// ShareService shares this computer's home line. Turning Share on checks
// the member's identity, registers this computer's agent key as a device,
// creates the device's line, turns sharing on at the API and runs the
// sharing agent, which dials out to the line's gateway.
type ShareService struct {
	api     *apiclient.Client
	profile *profile.Profile
	opts    ShareOptions
	log     *slog.Logger

	publishMu sync.Mutex // orders publications
	published ShareStatus

	// Listeners hear about changes from their own goroutine, in order, so
	// one that waits for the main thread, as the tray's menu does, never
	// holds up Share, not even ServiceShutdown, which Wails runs on the
	// main thread.
	listenMu  sync.Mutex
	listeners []func(ShareStatus)
	queue     []ShareStatus
	delivery  chan struct{} // nudges the delivery goroutine; nil until OnStatus
	closed    chan struct{} // closed by ServiceShutdown: delivery ends
	closeOnce sync.Once

	background     sync.WaitGroup // ServiceStartup's goroutines
	stopBackground context.CancelFunc

	// Each time Share turns on starts a run in its own goroutine. Locking
	// follows AuthService: mu is never held across an API call or while
	// waiting for a run, and a run writes back only while gen is still the
	// one it started with. A run waits for the previous run and its
	// turn-off to finish, so the API sees sharing turned on and off in
	// order.
	mu        sync.Mutex
	gen       uint64    // bumped whenever Share turns on or off
	on        bool      // the switch
	err       ErrorCode // why the run is offline, or why it stopped
	agent     Agent     // the running agent, or nil
	seen      uint64    // the agent's byte count when last sampled
	cancelRun context.CancelFunc
	runDone   chan struct{} // closed when the current run ends
	last      chan struct{} // closed when the latest run and its turn-off are done
	memberID  string        // whose Share the day count belongs to
	day       string        // the day dayBytes counts, YYYY-MM-DD
	dayBytes  uint64
}

// NewShareService returns a ShareService that calls api and keeps the
// agent's key and the line in p.
func NewShareService(api *apiclient.Client, p *profile.Profile, opts ShareOptions) *ShareService {
	if opts.NewAgent == nil {
		opts.NewAgent = func(cfg agent.Config) (Agent, error) { return agent.New(cfg) }
	}
	if opts.DeviceName == "" {
		opts.DeviceName = hostDeviceName()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RetryMin <= 0 {
		opts.RetryMin = time.Second
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = time.Minute
	}
	opts.RetryMax = max(opts.RetryMax, opts.RetryMin)
	if opts.StatusInterval <= 0 {
		opts.StatusInterval = time.Second
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	opts.Logger = log
	return &ShareService{
		api: api, profile: p, opts: opts, log: log,
		// Listeners hear about changes from the start state, off.
		published: ShareStatus{State: ShareOff},
		closed:    make(chan struct{}),
	}
}

// Status returns Share's state and today's count.
func (s *ShareService) Status() ShareStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

// SetSharing turns Share on or off and returns the status that follows.
// Turning on waits for the first attempt to set up the line: the answer is
// connecting, offline (it keeps trying) or error (Share stays off).
// Turning off stops the agent at once, then tells the API.
func (s *ShareService) SetSharing(ctx context.Context, on bool) ShareStatus {
	if on {
		return s.start(ctx)
	}
	s.stop(ctx, true)
	return s.Status()
}

// OnStatus calls fn with every change of the status, in order, from a
// goroutine of its own.
//
//wails:ignore
func (s *ShareService) OnStatus(fn func(ShareStatus)) {
	s.listenMu.Lock()
	defer s.listenMu.Unlock()
	s.listeners = append(s.listeners, fn)
	if s.delivery == nil {
		s.delivery = make(chan struct{}, 1)
		go s.deliver(s.delivery)
	}
}

// deliver hands queued statuses to the listeners until ServiceShutdown.
func (s *ShareService) deliver(nudge <-chan struct{}) {
	for {
		select {
		case <-nudge:
		case <-s.closed:
			return
		}
		for {
			s.listenMu.Lock()
			if len(s.queue) == 0 {
				s.listenMu.Unlock()
				break
			}
			st := s.queue[0]
			s.queue = s.queue[1:]
			listeners := s.listeners
			s.listenMu.Unlock()
			for _, fn := range listeners {
				fn(st)
			}
		}
	}
}

// SignOut turns Share off before the member signs out, while the session
// still works to tell the API, and forgets the member's day count.
//
//wails:ignore
func (s *ShareService) SignOut(ctx context.Context) {
	s.stop(ctx, true)
	s.mu.Lock()
	s.memberID, s.day, s.dayBytes = "", "", 0
	s.mu.Unlock()
	s.publish()
}

// ServiceStartup starts publishing the status and, if the member left Share
// on when the app last quit, turns it on again.
func (s *ShareService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.stopBackground = cancel
	s.mu.Unlock()
	s.background.Go(func() { s.watch(ctx) })
	s.background.Go(func() { s.resume(ctx) })
	return nil
}

// ServiceShutdown stops the agent when the app quits. The member's choice
// stays as it was, so Share comes back on at the next start.
func (s *ShareService) ServiceShutdown() error {
	s.mu.Lock()
	if s.stopBackground != nil {
		s.stopBackground()
	}
	s.mu.Unlock()
	s.background.Wait()
	s.stop(context.Background(), false)
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

// start turns Share on and returns the status after the first attempt to
// set up the line, or when ctx ends.
func (s *ShareService) start(ctx context.Context) ShareStatus {
	s.mu.Lock()
	if s.on {
		st := s.statusLocked()
		s.mu.Unlock()
		return st
	}
	if s.cancelRun != nil {
		s.cancelRun() // a run that failed and is ending
	}
	s.gen++
	gen := s.gen
	s.on, s.err = true, ""
	runCtx, cancel := context.WithCancel(context.Background())
	done, answered := make(chan struct{}), make(chan ShareStatus, 1)
	prev := s.last
	s.cancelRun, s.runDone, s.last = cancel, done, done
	s.mu.Unlock()
	s.publish()

	go s.run(runCtx, gen, prev, done, answered)
	select {
	case st := <-answered:
		return st
	case <-ctx.Done():
		return s.Status()
	}
}

// stop ends the current run. With turnOff, it records the member's choice
// and turns sharing off at the API; without, the app is quitting.
func (s *ShareService) stop(ctx context.Context, turnOff bool) {
	s.mu.Lock()
	s.gen++
	cancel, done, memberID := s.cancelRun, s.runDone, s.memberID
	s.on, s.err = false, ""
	s.cancelRun, s.runDone = nil, nil
	settled := make(chan struct{})
	defer close(settled)
	if done != nil {
		s.last = settled
	}
	s.mu.Unlock()
	if done == nil {
		s.publish()
		return
	}

	cancel()
	<-done // the run detached its agent and saved the day count
	s.publish()
	if !turnOff || memberID == "" {
		return
	}
	sh, err := s.updateShare(memberID, func(sh *profile.Share) { sh.On = false })
	if err != nil || !sh.APIOn || sh.LineID == "" {
		return
	}
	sess, ok, _ := s.profile.Session()
	if !ok {
		return
	}
	ctx, cancelPatch := context.WithTimeout(context.WithoutCancel(ctx), turnOffTimeout)
	defer cancelPatch()
	if err := s.api.UpdateLine(ctx, sess.Token, sh.LineID, false); err != nil {
		// The agent is stopped, so nothing flows through the line. The
		// next start tells the API (resume).
		s.log.Warn("could not turn sharing off at the API", "error", err)
		return
	}
	_, _ = s.updateShare(memberID, func(sh *profile.Share) { sh.APIOn = false })
}

// turnOffTimeout bounds telling the API that sharing is off.
const turnOffTimeout = 5 * time.Second

// run is one Share session, from on until stop or a failure.
// It answers start with the status after its first attempt.
func (s *ShareService) run(ctx context.Context, gen uint64, prev <-chan struct{}, done chan struct{}, answered chan<- ShareStatus) {
	defer close(done)
	answer := sync.OnceFunc(func() { answered <- s.Status() })
	defer answer()
	if prev != nil {
		select {
		case <-prev:
		case <-ctx.Done():
			return
		}
	}

	sess, ok, err := s.profile.Session()
	if err != nil || !ok {
		s.fail(gen, &Error{Code: ErrSignedOut, err: err})
		return
	}
	if !s.begin(gen, sess.MemberID) {
		return
	}

	retry := newRetry(s.opts.RetryMin, s.opts.RetryMax)
	ids := newEnrollIDs()
	var e enrollment
	for {
		e, err = s.enroll(ctx, sess, &ids)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		if !shareRetryable(err) {
			s.fail(gen, err)
			return
		}
		s.log.Warn("could not set up the line; trying again", "error", err)
		s.setOffline(gen, shareErrorCode(err))
		answer()
		select {
		case <-time.After(retry.next()):
		case <-ctx.Done():
			return
		}
	}

	a, err := s.opts.NewAgent(agent.Config{
		GatewayAddr: e.gatewayAddr,
		GatewayPin:  e.gatewayPin,
		Certificate: e.cert,
		Logger:      s.log,
	})
	if err != nil {
		s.fail(gen, &Error{Code: ErrUnexpected, err: err})
		return
	}
	if !s.attach(gen, a) {
		return
	}
	answer()
	err = a.Run(ctx)
	s.detach(a)
	if ctx.Err() == nil {
		s.fail(gen, &Error{Code: ErrUnexpected, err: fmt.Errorf("agent stopped: %w", err)})
	}
}

// begin records whose Share the run is and that they want it on. It
// reports false if Share was turned off meanwhile.
func (s *ShareService) begin(gen uint64, memberID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return false
	}
	sh, err := s.updateShareLocked(memberID, func(sh *profile.Share) { sh.On = true })
	if err != nil {
		s.log.Warn("could not save the Share setting", "error", err)
	}
	if s.memberID != memberID {
		s.memberID, s.day, s.dayBytes = memberID, "", 0
		if sh.Day == s.today() {
			s.day, s.dayBytes = sh.Day, sh.DayBytes
		}
	}
	return true
}

func (s *ShareService) attach(gen uint64, a Agent) bool {
	s.mu.Lock()
	if gen != s.gen {
		s.mu.Unlock()
		return false
	}
	s.agent, s.seen, s.err = a, 0, ""
	s.mu.Unlock()
	s.publish()
	return true
}

// detach takes the agent's last count and saves the day count.
func (s *ShareService) detach(a Agent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent != a {
		return
	}
	s.sampleLocked(a.Stats())
	s.agent, s.seen = nil, 0
	day, dayBytes := s.day, s.dayBytes
	if _, err := s.updateShareLocked(s.memberID, func(sh *profile.Share) {
		sh.Day, sh.DayBytes = day, dayBytes
	}); err != nil {
		s.log.Warn("could not save today's count", "error", err)
	}
}

func (s *ShareService) setOffline(gen uint64, code ErrorCode) {
	s.mu.Lock()
	if gen != s.gen {
		s.mu.Unlock()
		return
	}
	s.err = code
	s.mu.Unlock()
	s.publish()
}

// fail turns Share off because of err, which trying again won't fix.
func (s *ShareService) fail(gen uint64, err error) {
	s.mu.Lock()
	if gen != s.gen {
		s.mu.Unlock()
		return
	}
	s.log.Warn("Share stopped", "error", err)
	s.on, s.err = false, shareErrorCode(err)
	if s.memberID != "" {
		if _, err := s.updateShareLocked(s.memberID, func(sh *profile.Share) { sh.On = false }); err != nil {
			s.log.Warn("could not save the Share setting", "error", err)
		}
	}
	s.mu.Unlock()
	s.publish()
}

// updateShare changes what the profile remembers about member's Share.
func (s *ShareService) updateShare(memberID string, f func(*profile.Share)) (profile.Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateShareLocked(memberID, f)
}

// updateShareLocked holds mu, so the profile's Share file is read and
// written by one goroutine at a time. It is a small local file.
func (s *ShareService) updateShareLocked(memberID string, f func(*profile.Share)) (profile.Share, error) {
	sh, err := s.profile.Share(memberID)
	if err != nil {
		return sh, err
	}
	f(&sh)
	return sh, s.profile.SaveShare(memberID, sh)
}

func (s *ShareService) today() string { return s.opts.Now().Format(time.DateOnly) }

// sampleLocked adds what the agent relayed since the last sample to the
// day count.
func (s *ShareService) sampleLocked(st agent.Stats) {
	total := st.BytesIn + st.BytesOut
	if total < s.seen {
		s.seen = 0
	}
	delta := total - s.seen
	s.seen = total
	if today := s.today(); s.day != today {
		s.day, s.dayBytes = today, 0
	}
	s.dayBytes += delta
}

func (s *ShareService) statusLocked() ShareStatus {
	st := ShareStatus{Sharing: s.on, Error: s.err}
	var as agent.Stats
	if s.agent != nil {
		as = s.agent.Stats()
		s.sampleLocked(as)
	}
	if s.day == s.today() {
		st.BytesToday = s.dayBytes
	}
	switch {
	case !s.on && s.err != "":
		st.State = ShareError
	case !s.on:
		st.State = ShareOff
	case s.agent == nil && s.err != "":
		st.State = ShareOffline
	case s.agent == nil:
		st.State = ShareConnecting
	default:
		st.FlowsActive = as.FlowsActive
		switch {
		case as.OtherVPN:
			st.State = SharePaused
		case as.State == agent.StateConnected:
			st.State = ShareOnline
		case as.LastError != nil:
			st.State, st.Error = ShareOffline, ErrOffline
		default:
			st.State = ShareConnecting
		}
	}
	return st
}

// publish queues a changed status for the listeners.
func (s *ShareService) publish() {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	st := s.Status()
	if st == s.published {
		return
	}
	s.published = st
	s.listenMu.Lock()
	defer s.listenMu.Unlock()
	if s.delivery == nil {
		return // nobody listens
	}
	s.queue = append(s.queue, st)
	select {
	case s.delivery <- struct{}{}:
	default: // already nudged
	}
}

func (s *ShareService) watch(ctx context.Context) {
	t := time.NewTicker(s.opts.StatusInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.publish()
		case <-ctx.Done():
			return
		}
	}
}

// resume turns Share on at start-up if the member left it on, and tells
// the API about a turn-off it missed.
func (s *ShareService) resume(ctx context.Context) {
	sess, ok, err := s.profile.Session()
	if err != nil || !ok {
		return
	}
	sh, err := s.profile.Share(sess.MemberID)
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.memberID == "" {
		s.memberID = sess.MemberID
		if sh.Day == s.today() {
			s.day, s.dayBytes = sh.Day, sh.DayBytes
		}
	}
	s.mu.Unlock()
	switch {
	case sh.On:
		s.start(ctx)
	case sh.APIOn && sh.LineID != "":
		ctx, cancel := context.WithTimeout(ctx, turnOffTimeout)
		defer cancel()
		if s.api.UpdateLine(ctx, sess.Token, sh.LineID, false) == nil {
			_, _ = s.updateShare(sess.MemberID, func(sh *profile.Share) { sh.APIOn = false })
		}
	default:
		s.publish()
	}
}

// enrollment is what the agent needs to dial the line's gateway.
type enrollment struct {
	gatewayAddr string
	gatewayPin  relayprotocol.Pin
	cert        tls.Certificate
}

// enrollIDs are the request_ids of one run's POSTs, reused on every retry.
type enrollIDs struct{ device, line string }

func newEnrollIDs() enrollIDs {
	return enrollIDs{device: uuid.NewV7().String(), line: uuid.NewV7().String()}
}

// enroll sets up this computer's line for the signed-in member and turns
// sharing on at the API.
func (s *ShareService) enroll(ctx context.Context, sess profile.Session, ids *enrollIDs) (enrollment, error) {
	me, err := s.api.Me(ctx, sess.Token)
	if err != nil {
		return enrollment{}, err
	}
	if me.IdentityStatus != "verified" {
		return enrollment{}, &Error{Code: ErrIdentityRequired}
	}
	key, err := s.profile.AgentKey(sess.MemberID)
	if err != nil {
		return enrollment{}, &Error{Code: ErrUnexpected, err: err}
	}
	sh, err := s.updateShare(sess.MemberID, func(*profile.Share) {})
	if err != nil {
		return enrollment{}, &Error{Code: ErrUnexpected, err: err}
	}

	var line apiclient.LineCreated
	for registered := false; ; {
		if sh.DeviceID == "" {
			pub := base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
			dev, err := s.api.RegisterDevice(ctx, sess.Token, ids.device, s.opts.DeviceName, platform(), pub)
			if err != nil {
				return enrollment{}, ids.refresh(err)
			}
			registered = true
			sh, err = s.updateShare(sess.MemberID, func(sh *profile.Share) { sh.DeviceID = dev.ID })
			if err != nil {
				return enrollment{}, &Error{Code: ErrUnexpected, err: err}
			}
		}
		line, err = s.api.CreateLine(ctx, sess.Token, ids.line, sh.DeviceID)
		if apiclient.IsStatus(err, http.StatusNotFound) && !registered {
			// The device was removed at the API: register the key again,
			// which gives a new device and then a new line.
			*ids = newEnrollIDs()
			sh, err = s.updateShare(sess.MemberID, func(sh *profile.Share) {
				sh.DeviceID, sh.LineID, sh.APIOn = "", "", false
			})
			if err != nil {
				return enrollment{}, &Error{Code: ErrUnexpected, err: err}
			}
			continue
		}
		if err != nil {
			return enrollment{}, ids.refresh(err)
		}
		break
	}
	pin, err := gatewayPin(line.Gateway.SPKISHA256)
	if err != nil {
		return enrollment{}, &Error{Code: ErrUnexpected, err: err}
	}
	if line.Gateway.QUICEndpoint == "" {
		return enrollment{}, &Error{Code: ErrUnexpected, err: errors.New("the line has no gateway endpoint")}
	}
	if _, err := s.updateShare(sess.MemberID, func(sh *profile.Share) { sh.LineID = line.ID }); err != nil {
		return enrollment{}, &Error{Code: ErrUnexpected, err: err}
	}
	if err := s.api.UpdateLine(ctx, sess.Token, line.ID, true); err != nil {
		return enrollment{}, err
	}
	if _, err := s.updateShare(sess.MemberID, func(sh *profile.Share) { sh.APIOn = true }); err != nil {
		return enrollment{}, &Error{Code: ErrUnexpected, err: err}
	}
	cert, err := relayprotocol.NewSelfSigned(key, "homeport-agent")
	if err != nil {
		return enrollment{}, &Error{Code: ErrUnexpected, err: err}
	}
	return enrollment{gatewayAddr: line.Gateway.QUICEndpoint, gatewayPin: pin, cert: cert}, nil
}

// refresh replaces the run's request_ids once the API says they are too
// old to replay; the POSTs are safe to send anew, because registering a key
// and creating a device's line return what exists.
func (ids *enrollIDs) refresh(err error) error {
	var apiErr *apiclient.Error
	if errors.As(err, &apiErr) && apiErr.Code == "request_id_expired" {
		*ids = newEnrollIDs()
		return &retryableError{err}
	}
	return err
}

// retryableError marks an error that trying again fixes.
type retryableError struct{ error }

func (e *retryableError) Unwrap() error { return e.error }

// gatewayPin turns the contract's pin, the hex SHA-256 of the gateway's
// SPKI, into relay-protocol's Pin, whose text form is base64.
func gatewayPin(spkiSHA256 string) (relayprotocol.Pin, error) {
	b, err := hex.DecodeString(spkiSHA256)
	if err != nil || len(b) != len(relayprotocol.Pin{}) {
		return relayprotocol.Pin{}, fmt.Errorf("gateway pin %q is not a hex SHA-256", spkiSHA256)
	}
	return relayprotocol.Pin(b), nil
}

// shareRetryable reports whether setting up the line may work if tried
// again: the API couldn't be reached, failed, or asked to slow down.
func shareRetryable(err error) bool {
	var re *retryableError
	if errors.As(err, &re) {
		return true
	}
	var svcErr *Error
	if errors.As(err, &svcErr) {
		return false
	}
	return retryable(err)
}

// shareErrorCode explains a Share failure to the member.
func shareErrorCode(err error) ErrorCode {
	var svcErr *Error
	if errors.As(err, &svcErr) {
		return svcErr.Code
	}
	var apiErr *apiclient.Error
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == http.StatusUnauthorized:
			return ErrSignedOut
		case apiErr.Code == "identity_required":
			return ErrIdentityRequired
		case apiErr.Code == "gateway_unavailable":
			return ErrGatewayUnavailable
		}
	}
	return serviceError(err).(*Error).Code
}

// platform is this computer's platform as the contract names it.
func platform() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	default:
		return "linux"
	}
}

// hostDeviceName names this computer for the member's devices: its host
// name without .local, at most 64 characters.
func hostDeviceName() string {
	name, err := os.Hostname()
	name = strings.TrimSuffix(strings.TrimSpace(name), ".local")
	if err != nil || name == "" {
		return "This computer"
	}
	if r := []rune(name); len(r) > 64 {
		name = string(r[:64])
	}
	return name
}

// retry is a doubling delay between attempts.
type retry struct{ cur, min, max time.Duration }

func newRetry(lo, hi time.Duration) *retry { return &retry{min: lo, max: hi} }

func (r *retry) next() time.Duration {
	if r.cur == 0 {
		r.cur = r.min
	} else {
		r.cur = min(r.cur*2, r.max)
	}
	return r.cur
}
