// Command member does what the desktop app's Use mode will do, for manual
// runs against a local API: sign in, check identity, buy or grant credits,
// list lines and create a WireGuard peer.
//
// Usage:
//
//	member [-api URL] [-session FILE] COMMAND
//
// Commands:
//
//	signin EMAIL [-dev]  sign in; open the link from the wrangler dev log,
//	                     or approve at once through the dev route with -dev
//	me                   identity status and credits
//	identity             start a Stripe Identity check and print its page
//	checkout             start buying a credit pack and print its page
//	verify               mark the identity verified (dev route)
//	credits N            grant N purchased credits (dev route)
//	lines                list the lines this member may use
//	peer LINE_ID         add a WireGuard peer on a line and print its .conf,
//	                     private key included
//	share KEY_FILE       share this machine's line with the agent CLI: register
//	                     its key (homeport-agent -print-pin -key KEY_FILE makes
//	                     one), create the line, turn sharing on, and print the
//	                     agent's -gateway and -gateway-pin
//
// The session file has the desktop app's session.json format, so -session
// can point into an app profile to act as that profile's member.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/jermainee/homeport/e2e/internal/memberapi"
	"github.com/jermainee/homeport/e2e/internal/wgclient"
	"github.com/jermainee/homeport/gateway"
	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// session is the desktop app's session.json (desktop-app/internal/profile).
type session struct {
	Token     string    `json:"token"`
	MemberID  string    `json:"member_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("member", flag.ContinueOnError)
	flags.SetOutput(stderr)
	api := flags.String("api", "http://localhost:8787", "API base URL")
	// The default stays out of the repo, so a session token is never committed.
	sessionPath := flags.String("session", filepath.Join(os.TempDir(), "homeport-member.json"), "session file (an app profile's session.json works too)")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: member [-api URL] [-session FILE] signin EMAIL [-dev] | me | identity | checkout | verify | credits N | lines | peer LINE_ID | share KEY_FILE")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() == 0 {
		flags.Usage()
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	c := &memberapi.Client{BaseURL: *api}
	cmd, rest := flags.Arg(0), flags.Args()[1:]

	if cmd == "signin" {
		if err := signIn(ctx, c, *sessionPath, rest, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "member: %v\n", err)
			return 1
		}
		return 0
	}
	s, err := loadSession(*sessionPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "member: %v (run member signin first)\n", err)
		return 1
	}
	c.Token = s.Token
	if err := command(ctx, c, s, cmd, rest, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "member: %v\n", err)
		return 1
	}
	return 0
}

func command(ctx context.Context, c *memberapi.Client, s session, cmd string, args []string, stdout, stderr io.Writer) error {
	switch {
	case cmd == "me" && len(args) == 0:
		me, err := c.Me(ctx)
		if err != nil {
			return err
		}
		return printJSON(stdout, me)
	case cmd == "identity" && len(args) == 0:
		url, err := c.StartIdentityCheck(ctx)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(stdout, url)
		return nil
	case cmd == "checkout" && len(args) == 0:
		url, err := c.StartCheckout(ctx)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(stdout, url)
		return nil
	case cmd == "verify" && len(args) == 0:
		return c.DevVerifyIdentity(ctx, s.MemberID)
	case cmd == "credits" && len(args) == 1:
		n, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("credits: %q is not a positive whole number", args[0])
		}
		return c.DevGrantCredits(ctx, s.MemberID, n)
	case cmd == "lines" && len(args) == 0:
		lines, err := c.AvailableLines(ctx)
		if err != nil {
			return err
		}
		return printJSON(stdout, lines)
	case cmd == "peer" && len(args) == 1:
		key, err := gateway.GenerateKey()
		if err != nil {
			return err
		}
		host, _ := os.Hostname()
		peer, err := c.CreatePeer(ctx, args[0], "member tool on "+truncate(host, 40), key.PublicKey().String())
		if err != nil {
			return err
		}
		_, _ = fmt.Fprint(stdout, wgclient.WithPrivateKey(peer.ConfigText, key))
		_, _ = fmt.Fprintf(stderr, "member: peer %s, tunnel address %s. The .conf holds its private key; keep it to yourself.\n", peer.Peer.ID, peer.Peer.IPv4)
		return nil
	case cmd == "share" && len(args) == 1:
		return share(ctx, c, args[0], stdout)
	}
	return fmt.Errorf("unknown command or wrong arguments: %s %v", cmd, args)
}

// share does what the app's Share switch does, for the agent CLI's key.
func share(ctx context.Context, c *memberapi.Client, keyPath string, stdout io.Writer) error {
	key, err := loadAgentKey(keyPath)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	device, err := c.RegisterDevice(ctx, truncate(host, 64), platform(), key.Public().(ed25519.PublicKey))
	if err != nil {
		return err
	}
	line, err := c.CreateLine(ctx, device.ID)
	if err != nil {
		return err
	}
	if err := c.SetSharing(ctx, line.ID, true); err != nil {
		return err
	}
	pin, err := hex.DecodeString(line.Gateway.SPKISHA256)
	if err != nil || len(pin) != len(relayprotocol.Pin{}) {
		return fmt.Errorf("the API sent the gateway pin %q", line.Gateway.SPKISHA256)
	}
	_, _ = fmt.Fprintf(stdout, "# line %s\nHOMEPORT_GATEWAY=%s\nHOMEPORT_GATEWAY_PIN=%s\n",
		line.ID, line.Gateway.QUICEndpoint, relayprotocol.Pin(pin))
	return nil
}

// loadAgentKey reads the agent CLI's key file, a PEM PKCS #8 Ed25519 key.
func loadAgentKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the operator's own argument
	if err != nil {
		return nil, fmt.Errorf("%w (homeport-agent -print-pin -key %s creates it)", err, path)
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s holds no PEM PRIVATE KEY", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s holds a %T, want an Ed25519 key", path, parsed)
	}
	return key, nil
}

// platform is this machine's platform, as the API names it.
func platform() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

func signIn(ctx context.Context, c *memberapi.Client, path string, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("signin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dev := fs.Bool("dev", false, "approve at once through the dev route (ENVIRONMENT=dev)")
	// The email comes first, so parse the flags after it.
	if len(args) == 0 {
		return errors.New("usage: member signin EMAIL [-dev]")
	}
	email := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	c.Token = memberapi.NewSessionToken()
	lr, err := c.StartLogin(ctx, email)
	if err != nil {
		return err
	}
	if *dev {
		if err := c.DevConfirm(ctx, lr.ID); err != nil {
			return err
		}
	} else {
		_, _ = fmt.Fprintf(stderr, "member: match code %s. Open the sign-in link for %s from the wrangler dev log and confirm.\n", lr.MatchCode, email)
	}
	waitCtx, cancel := context.WithDeadline(ctx, lr.ExpiresAt)
	defer cancel()
	memberID, err := c.WaitLogin(waitCtx, lr.ID, 2*time.Second)
	if err != nil {
		return err
	}
	if err := saveSession(path, session{Token: c.Token, MemberID: memberID}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "member: signed in as %s; session in %s\n", memberID, path)
	return nil
}

func loadSession(path string) (session, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the operator's own flag
	if err != nil {
		return session{}, err
	}
	var s session
	if err := json.Unmarshal(b, &s); err != nil || s.Token == "" || s.MemberID == "" {
		return session{}, fmt.Errorf("%s is not a session", path)
	}
	return s, nil
}

// saveSession writes the session readable by the user only, since its
// token signs in as the member.
func saveSession(path string, s session) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600) // WriteFile keeps an existing file's mode
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
