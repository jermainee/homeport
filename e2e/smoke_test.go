//go:build e2e && smoke && unix

package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jermainee/homeport/agent"
	"github.com/jermainee/homeport/e2e/internal/memberapi"
	"github.com/jermainee/homeport/e2e/internal/wgclient"
	"github.com/jermainee/homeport/gateway"
	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// The smoke test's transfers, and what member B starts with.
const (
	smokeDownload = 3_000_000
	smokeUpload   = 1_000_000
	smokeWgcurl   = 1_000_000
	smokeCredits  = 1_000_000

	// Prices from api/src/pricing.ts, per 10^6 bytes, upload plus download.
	useCreditsPerMB   = 10
	shareCreditsPerMB = 5
)

// TestFullStackSmoke runs Homeport the way members meet it, all on this
// machine: the real control API (wrangler dev on a fresh local D1), the
// gateway binary, member A's sharing agent and member B's WireGuard client.
// B fetches through A's line, and the money path is checked end to end:
// bytes through the tunnel, then the gateway's usage reports, then ledger
// entries.
//
// The agent and the client run in this process. The agent needs the e2e
// build tag's RouteTestOrigin to reach the loopback origin, which the
// agent CLI refuses on purpose; the client's tun counts every byte.
//
//	cd api && npm ci && cd ../e2e
//	GOWORK=off go test -count=1 -tags e2e,smoke -run TestFullStackSmoke -v .
func TestFullStackSmoke(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	origin := startOrigin(t)

	worker := startWorker(t, dir)
	gwBin := goBuild(t, dir, "github.com/jermainee/homeport/gateway/cmd/homeport-gateway")
	wgcurlBin := goBuild(t, dir, "github.com/jermainee/homeport/e2e/cmd/wgcurl")
	gw := newGatewayRunner(t, gwBin, worker, dir)
	gw.start()

	// Two members: A shares a line, B uses it.
	a := &memberapi.Client{BaseURL: worker.url}
	aID := signedInVerified(ctx, t, a, "a@smoke.homeport.test")
	b := &memberapi.Client{BaseURL: worker.url}
	bID := signedInVerified(ctx, t, b, "b@smoke.homeport.test")
	if err := b.DevGrantCredits(ctx, bID, smokeCredits); err != nil {
		t.Fatal(err)
	}

	// A's line, as the app's Share switch creates it.
	agentKey, agentCert := newAgentIdentity(t)
	device, err := a.RegisterDevice(ctx, "Smoke A", memberapi.Platform(), agentKey.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if pin := relayprotocol.PinOf(agentCert.Leaf); device.SPKISHA256 != hex.EncodeToString(pin[:]) {
		t.Fatalf("the API's SPKI hash %s is not the agent's pin %x", device.SPKISHA256, pin[:])
	}
	line, err := a.CreateLine(ctx, device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetSharing(ctx, line.ID, true); err != nil {
		t.Fatal(err)
	}

	// B's WireGuard peer on A's line.
	clientKey, err := gateway.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	peer, err := b.CreatePeer(ctx, line.ID, "Smoke B", clientKey.PublicKey().String())
	if err != nil {
		t.Fatal(err)
	}
	// A new device gets HTTPS only. The smoke test's origin speaks plain
	// HTTP, so B turns it off, as a member who needs plain HTTP would.
	if !peer.Peer.HTTPSOnly {
		t.Error("a new peer does not have HTTPS only")
	}
	if err := b.SetHTTPSOnly(ctx, peer.Peer.ID, false); err != nil {
		t.Fatal(err)
	}

	t.Run("no self-dealing: B gets no peer on B's own line", func(t *testing.T) {
		bAgent, _ := newAgentIdentity(t)
		bDevice, err := b.RegisterDevice(ctx, "Smoke B", memberapi.Platform(), bAgent.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		bLine, err := b.CreateLine(ctx, bDevice.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetSharing(ctx, bLine.ID, true); err != nil {
			t.Fatal(err)
		}
		key, err := gateway.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		_, err = b.CreatePeer(ctx, bLine.ID, "Own line", key.PublicKey().String())
		var apiErr *memberapi.Error
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden || apiErr.Code != "line_not_allowed" {
			t.Fatalf("a peer on B's own line: %v, want 403 line_not_allowed", err)
		}
	})

	// A's agent dials the gateway the API named for the line. The gateway
	// fetched its config before A turned sharing on, so it refuses the
	// agent's first attempt and fetches its config again at once. That
	// fetch brings B's peer too.
	gwPin, err := hex.DecodeString(line.Gateway.SPKISHA256)
	if err != nil || len(gwPin) != len(relayprotocol.Pin{}) {
		t.Fatalf("the line's gateway pin %q", line.Gateway.SPKISHA256)
	}
	agentLogs := &syncBuffer{}
	cfg := agent.Config{
		GatewayAddr:  line.Gateway.QUICEndpoint,
		GatewayPin:   relayprotocol.Pin(gwPin),
		Certificate:  agentCert,
		Logger:       slog.New(slog.NewJSONHandler(agentLogs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		ReconnectMin: 100 * time.Millisecond,
		ReconnectMax: time.Second,
		RouteCheck:   func(string) (agent.Route, error) { return agent.Route{}, nil },
	}
	agent.RouteTestOrigin(&cfg, tcpDst, origin.tcp)
	ag, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	agentCtx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan error, 1)
	agentStarted := time.Now()
	go func() { agentDone <- ag.Run(agentCtx) }()
	t.Cleanup(func() {
		stopAgent()
		select {
		case err := <-agentDone:
			if err != nil {
				t.Errorf("agent: %v", err)
			}
		case <-time.After(waitFor):
			t.Error("the agent did not stop")
		}
	})
	waitUntil(t, "A's agent serves the line", func() bool {
		st := ag.Stats()
		return st.State == agent.StateConnected && st.LineID == line.ID
	})
	if took := time.Since(agentStarted); took > 5*time.Second {
		t.Errorf("A's agent served the line %v after it started; want a few seconds, not the gateway's 30 s config poll", took)
	} else {
		t.Logf("A's agent served the line %v after it started", took.Round(time.Millisecond))
	}

	// B's client runs the .conf the API handed out, with B's own key.
	conf := wgclient.WithPrivateKey(peer.ConfigText, clientKey)
	clientCfg, err := wgclient.ParseConfig(conf)
	if err != nil {
		t.Fatal(err)
	}
	if clientCfg.Endpoint != gw.wgEndpoint {
		t.Fatalf("the config's endpoint %s is not the gateway's %s", clientCfg.Endpoint, gw.wgEndpoint)
	}
	client, err := wgclient.New(clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	httpClient := &http.Client{
		Timeout:   transferTimeout,
		Transport: &http.Transport{DialContext: client.Net.DialContext, DisableKeepAlives: true},
	}

	if !t.Run("B fetches through A's line, intact", func(t *testing.T) {
		resp, err := httpClient.Get(fmt.Sprintf("http://%s%s/blob/%d", tcpDst, contentPath, smokeDownload))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("download: %d, %v", resp.StatusCode, err)
		}
		if sum(body) != sum(blob(smokeDownload)) {
			t.Errorf("the download's SHA-256 is %s, want %s", sum(body), sum(blob(smokeDownload)))
		}

		up := blob(smokeUpload)
		resp, err = httpClient.Post(fmt.Sprintf("http://%s%s/upload", tcpDst, contentPath), "application/octet-stream", bytes.NewReader(up))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || string(got) != sum(up) {
			t.Errorf("the origin got an upload with SHA-256 %q (%d, %v), want %s", got, resp.StatusCode, err, sum(up))
		}
	}) {
		t.FailNow() // the money checks below would only repeat this failure
	}

	// Every inner byte, as B's tun counted it once the last ACKs landed.
	tun := waitStable(t, time.Second, func() [2]uint64 { return [2]uint64{client.Sent(), client.Received()} })
	tunnelBytes := int64(tun[0] + tun[1]) // #nosec G115 -- a few megabytes
	t.Logf("B's tun: %d bytes sent, %d received", tun[0], tun[1])
	client.Close()

	// The runbook's tool, with the same .conf: one more fetch, whose tunnel
	// bytes it reports.
	if !t.Run("wgcurl fetches with the API's .conf", func(t *testing.T) {
		n := runWgcurl(t, wgcurlBin, dir, conf, fmt.Sprintf("http://%s%s/blob/%d", tcpDst, contentPath, smokeWgcurl))
		t.Logf("wgcurl's tunnel: %d bytes", n)
		tunnelBytes += n
	}) {
		t.FailNow()
	}

	// SIGTERM: the gateway ends the session with a final usage event, then
	// returns the lease.
	gw.stop()

	// A restart keeps the gateway's keys in its state dir: A's agent, still
	// pinned to the key the API recorded at the first start, gets back in.
	waitUntil(t, "A's agent noticed the gateway stopped", func() bool {
		return ag.Stats().State != agent.StateConnected
	})
	gw.start()
	waitUntil(t, "A's agent serves the line on the restarted gateway", func() bool {
		st := ag.Stats()
		return st.State == agent.StateConnected && st.LineID == line.ID
	})
	gw.stop()

	db := worker.query(t,
		fmt.Sprintf(`SELECT COALESCE(SUM(bytes_up + bytes_down), 0) AS bytes FROM usage_sessions WHERE peer_id = '%s'`, sqlID(t, peer.Peer.ID)),
		`SELECT balance FROM ledger_accounts WHERE id = 'platform:platform_revenue'`,
		sqlUnbalancedTransactions,
		sqlDriftedBalances,
		`SELECT
			(SELECT COUNT(*) FROM leases) AS leases,
			(SELECT COUNT(*) FROM leases WHERE returned_at IS NULL) AS open_leases,
			(SELECT COUNT(*) FROM credit_reservations WHERE released_at IS NULL) AS open_reservations`,
	)
	usage, revenue, unbalanced, drifted, leases := db[0], db[1], db[2], db[3], db[4]

	t.Run("the gateway reported every byte B's tunnels counted", func(t *testing.T) {
		if got := int64(usage[0]["bytes"].(float64)); got != tunnelBytes {
			t.Errorf("the API recorded %d bytes for B's peer; B's tunnels counted %d", got, tunnelBytes)
		}
	})

	charged := tunnelBytes * useCreditsPerMB / 1_000_000
	earned := tunnelBytes * shareCreditsPerMB / 1_000_000
	t.Run("B paid 10 credits per MB", func(t *testing.T) {
		me, err := b.Me(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := memberapi.Credits{Purchased: smokeCredits - charged}
		if me.Credits != want {
			t.Errorf("B's credits are %+v, want %+v (%d bytes)", me.Credits, want, tunnelBytes)
		}
	})
	t.Run("A earned 5 credits per MB", func(t *testing.T) {
		me, err := a.Me(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := memberapi.Credits{EarnedPending: earned}
		if me.MemberID != aID || me.Credits != want {
			t.Errorf("A's credits are %+v, want %+v (%d bytes)", me.Credits, want, tunnelBytes)
		}
	})
	t.Run("the platform kept the difference", func(t *testing.T) {
		if got := int64(revenue[0]["balance"].(float64)); got != charged-earned {
			t.Errorf("platform revenue is %d, want %d", got, charged-earned)
		}
	})
	t.Run("every ledger transaction sums to zero", func(t *testing.T) {
		if len(unbalanced) > 0 {
			t.Errorf("unbalanced transactions: %v", unbalanced)
		}
		if len(drifted) > 0 {
			t.Errorf("accounts whose balance is not the sum of their entries: %v", drifted)
		}
	})
	t.Run("the gateway returned its leases", func(t *testing.T) {
		if r := leases[0]; r["leases"].(float64) < 1 || r["open_leases"].(float64) != 0 || r["open_reservations"].(float64) != 0 {
			t.Errorf("leases after the gateway stopped: %v", r)
		}
	})

	t.Run("no log names a destination", func(t *testing.T) {
		for name, logs := range map[string]string{
			"gateway": gw.logs.String(), "agent": agentLogs.String(), "worker": worker.logs.String(),
		} {
			for _, re := range smokeDestinationPatterns(origin, gw) {
				if loc := re.FindStringIndex(logs); loc != nil {
					start, end := max(loc[0]-200, 0), min(loc[1]+200, len(logs))
					t.Errorf("the %s's log names a destination (%s):\n%s", name, re, logs[start:end])
				}
			}
		}
	})
}

// sqlUnbalancedTransactions lists transactions whose entries do not sum to
// zero, or that have fewer than two entries. The runbook in README.md runs
// the same query.
const sqlUnbalancedTransactions = `SELECT t.id, t.kind, COALESCE(SUM(e.amount), 0) AS total, COUNT(e.id) AS entries
  FROM ledger_transactions t LEFT JOIN ledger_entries e ON e.transaction_id = t.id
 GROUP BY t.id HAVING total <> 0 OR entries < 2`

// sqlDriftedBalances lists accounts whose cached balance is not the sum of
// their entries.
const sqlDriftedBalances = `SELECT a.id, a.balance, COALESCE(SUM(e.amount), 0) AS total
  FROM ledger_accounts a LEFT JOIN ledger_entries e ON e.account_id = a.id
 GROUP BY a.id HAVING a.balance <> total`

func signedInVerified(ctx context.Context, t *testing.T, c *memberapi.Client, email string) string {
	t.Helper()
	id, err := c.SignInDev(ctx, email)
	if err != nil {
		t.Fatalf("sign in %s: %v", email, err)
	}
	if err := c.DevVerifyIdentity(ctx, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// newAgentIdentity is a sharing agent's key and certificate, as the app
// creates them.
func newAgentIdentity(t *testing.T) (ed25519.PrivateKey, tls.Certificate) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := relayprotocol.NewSelfSigned(key, "homeport-agent")
	if err != nil {
		t.Fatal(err)
	}
	return key, cert
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// sqlID returns id, an API id, after checking it is one, so it can stand
// in a query as is.
func sqlID(t *testing.T, id string) string {
	t.Helper()
	if !idPattern.MatchString(id) {
		t.Fatalf("%q is not an id", id)
	}
	return id
}

// smokeDestinationPatterns are what no log may hold: where B's flows went
// and what they asked for.
func smokeDestinationPatterns(o *origin, gw *gatewayRunner) []*regexp.Regexp {
	pats := []string{regexp.QuoteMeta(tcpDst.Addr().String()), regexp.QuoteMeta(contentPath)}
	// The origin's port, unless the gateway uses the same number for UDP.
	if p := o.tcp.Port(); p != gw.wgPort && p != gw.quicPort {
		port := strconv.Itoa(int(p))
		pats = append(pats, `[0-9.\]]:`+port+`\b`, `"(dst_)?port":"?`+port+`\b`)
	}
	out := make([]*regexp.Regexp, len(pats))
	for i, p := range pats {
		out[i] = regexp.MustCompile(p)
	}
	return out
}

// worker is the control API running in wrangler dev.
type worker struct {
	url     string
	token   string // the seeded dev gateway's token
	apiDir  string
	persist string // the local D1's directory
	logs    *syncBuffer
}

// startWorker seeds a fresh local D1 in dir, as npm run seed:dev does, and
// runs the Worker on it with wrangler dev, with the dev routes on.
func startWorker(t *testing.T, dir string) *worker {
	t.Helper()
	apiDir, err := filepath.Abs(filepath.Join("..", "api"))
	if err != nil {
		t.Fatal(err)
	}
	wrangler := filepath.Join(apiDir, "node_modules", ".bin", "wrangler")
	if _, err := os.Stat(wrangler); err != nil {
		t.Fatalf("no wrangler in %s: run npm ci in api/ first (%v)", apiDir, err)
	}
	w := &worker{apiDir: apiDir, persist: filepath.Join(dir, "d1"), logs: &syncBuffer{}}

	seed := exec.Command("npm", "run", "seed:dev", "--", "--persist-to", w.persist) // #nosec G204 -- the test's own temp dir
	seed.Dir = apiDir
	seed.Env = wranglerEnv()
	out, err := seed.CombinedOutput()
	if err != nil {
		t.Fatalf("npm run seed:dev: %v\n%s", err, out)
	}
	m := regexp.MustCompile(`HOMEPORT_GATEWAY_TOKEN=(hpgw_[A-Za-z0-9_.-]+)`).FindSubmatch(out)
	if m == nil {
		t.Fatalf("npm run seed:dev printed no gateway token:\n%s", out)
	}
	w.token = string(m[1])

	port := freeTCPPort(t)
	w.url = "http://127.0.0.1:" + strconv.Itoa(port)
	p := startProcess(t, "wrangler dev", apiDir, wranglerEnv(), w.logs, wrangler, "dev",
		"--local", "--persist-to", w.persist,
		"--ip", "127.0.0.1", "--port", strconv.Itoa(port),
		"--inspector-port", strconv.Itoa(freeTCPPort(t)),
		"--show-interactive-dev-session=false",
		"--var", "ENVIRONMENT:dev", "--var", "EMAIL_MODE:log", "--var", "PUBLIC_BASE_URL:"+w.url,
	)
	// wrangler's exit status after SIGINT says nothing about the test.
	t.Cleanup(func() { _ = p.stop(syscall.SIGINT) })

	probe := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		resp, err := probe.Get(w.url + "/v1/me")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				return w
			}
		}
		if p.exited() || time.Now().After(deadline) {
			t.Fatalf("wrangler dev did not come up on %s:\n%s", w.url, w.logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// query runs SQL statements on the Worker's local D1 in one wrangler call
// and returns each statement's rows. Numbers come back as float64.
func (w *worker) query(t *testing.T, statements ...string) [][]map[string]any {
	t.Helper()
	cmd := exec.Command(filepath.Join(w.apiDir, "node_modules", ".bin", "wrangler"), // #nosec G204 -- the test's own query
		"d1", "execute", "homeport", "--local", "--persist-to", w.persist, "--json", "--command", strings.Join(statements, ";\n"))
	cmd.Dir = w.apiDir
	cmd.Env = wranglerEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("wrangler d1 execute: %v\n%s", err, stderr.String())
	}
	var results []struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(out, &results); err != nil || len(results) != len(statements) {
		t.Fatalf("wrangler d1 execute printed %q: %v", out, err)
	}
	rows := make([][]map[string]any, len(results))
	for i, r := range results {
		rows[i] = r.Results
	}
	return rows
}

func wranglerEnv() []string {
	return append(os.Environ(), "WRANGLER_SEND_METRICS=false", "NO_COLOR=1", "FORCE_COLOR=0")
}

// goBuild builds a command into dir and returns its path.
func goBuild(t *testing.T, dir, pkg string) string {
	t.Helper()
	bin := filepath.Join(dir, filepath.Base(pkg))
	cmd := exec.Command("go", "build", "-o", bin, pkg) // #nosec G204 -- the test's own packages
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, out)
	}
	return bin
}

// gatewayRunner runs the gateway binary against the Worker, as an operator
// would, with one state dir and the same ports across restarts.
type gatewayRunner struct {
	t                *testing.T
	bin, api, token  string
	stateDir         string
	wgPort, quicPort uint16
	wgEndpoint       string
	logs             *syncBuffer
	proc             *process
}

func newGatewayRunner(t *testing.T, bin string, w *worker, dir string) *gatewayRunner {
	ports := freeUDPPorts(t, 2)
	g := &gatewayRunner{
		t: t, bin: bin, api: w.url, token: w.token,
		stateDir: filepath.Join(dir, "gateway-state"),
		wgPort:   ports[0],
		quicPort: ports[1],
		logs:     &syncBuffer{},
	}
	g.wgEndpoint = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(g.wgPort)))
	return g
}

// start runs the gateway and waits until the API accepted its announcement.
func (g *gatewayRunner) start() {
	t := g.t
	t.Helper()
	before := strings.Count(g.logs.String(), `"msg":"gateway running"`)
	quic := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(g.quicPort)))
	g.proc = startProcess(t, "gateway", "", append(os.Environ(), "HOMEPORT_GATEWAY_TOKEN="+g.token), g.logs, g.bin,
		"--api", g.api,
		"--wg-listen", ":"+strconv.Itoa(int(g.wgPort)), "--wg-endpoint", g.wgEndpoint,
		"--quic-listen", quic, "--quic-endpoint", quic,
		"--state-dir", g.stateDir,
	)
	p := g.proc
	// The test stops each run itself and checks its exit; this is for a
	// test that failed before that.
	t.Cleanup(func() { _ = p.stop(syscall.SIGTERM) })
	waitUntil(t, "the gateway announced itself", func() bool {
		if p.exited() {
			t.Fatalf("the gateway exited:\n%s", g.logs.String())
		}
		return strings.Count(g.logs.String(), `"msg":"gateway running"`) > before
	})
}

// stop sends SIGTERM and waits for a clean exit: final usage events, lease
// returns, lines offline.
func (g *gatewayRunner) stop() {
	g.t.Helper()
	if err := g.proc.stop(syscall.SIGTERM); err != nil {
		g.t.Fatalf("the gateway did not stop cleanly: %v\n%s", err, g.logs.String())
	}
}

// process is a child process in its own process group, so stopping it
// also stops what it started (wrangler runs workerd).
type process struct {
	name string
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startProcess(t *testing.T, name, dir string, env []string, logs io.Writer, prog string, args ...string) *process {
	t.Helper()
	cmd := exec.Command(prog, args...) // #nosec G204 -- the test's own programs
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = logs
	cmd.Stderr = logs
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	p := &process{name: name, cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p
}

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// stop signals the process group and waits up to 30 s, then kills it. It
// returns how the process ended; stopping it again returns the same.
func (p *process) stop(sig syscall.Signal) error {
	if !p.exited() {
		_ = syscall.Kill(-p.cmd.Process.Pid, sig)
		select {
		case <-p.done:
		case <-time.After(30 * time.Second):
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			<-p.done
			return fmt.Errorf("%s did not stop within 30 s", p.name)
		}
	}
	return p.err
}

// freeTCPPort returns a TCP port nothing listens on right now.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// freeUDPPorts returns n different UDP ports nothing listens on right now.
// It holds each until it has all, so it never returns one port twice.
func freeUDPPorts(t *testing.T, n int) []uint16 {
	t.Helper()
	ports := make([]uint16, n)
	for i := range ports {
		pc, err := net.ListenUDP("udp", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = pc.Close() }()
		ports[i] = pc.LocalAddr().(*net.UDPAddr).AddrPort().Port()
	}
	return ports
}

var (
	wgcurlSum   = regexp.MustCompile(`, (\d+) bytes, sha256 ([0-9a-f]{64})\n`)
	wgcurlBytes = regexp.MustCompile(`tunnel carried (\d+) bytes`)
)

// runWgcurl fetches url with the wgcurl binary and the .conf text, checks
// the body against the origin's blob, and returns the tunnel bytes wgcurl
// reported.
func runWgcurl(t *testing.T, bin, dir, conf, url string) int64 {
	t.Helper()
	confPath := filepath.Join(dir, "b.conf")
	if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "-conf", confPath, "-timeout", transferTimeout.String(), url) // #nosec G204 -- the test's own binary
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("wgcurl: %v\n%s", err, stderr.String())
	}
	m := wgcurlSum.FindStringSubmatch(stderr.String())
	if m == nil || m[1] != strconv.Itoa(smokeWgcurl) || m[2] != sum(blob(smokeWgcurl)) || sum(stdout.Bytes()) != m[2] {
		t.Fatalf("wgcurl got %d bytes with SHA-256 %s; it reported:\n%s", stdout.Len(), sum(stdout.Bytes()), stderr.String())
	}
	b := wgcurlBytes.FindStringSubmatch(stderr.String())
	if b == nil {
		t.Fatalf("wgcurl reported no tunnel bytes:\n%s", stderr.String())
	}
	n, err := strconv.ParseInt(b[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
