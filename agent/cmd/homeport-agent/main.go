// Command homeport-agent shares this machine's internet line through a
// Homeport gateway. It is for development and the Docker image; members use
// the desktop app, which embeds the same agent.
//
// Usage:
//
//	homeport-agent -gateway HOST:PORT -gateway-pin PIN [-key FILE] [-v]
//	homeport-agent -print-pin [-key FILE]
//
// The agent's identity is an Ed25519 key, created on first start. The
// gateway accepts the agent by the pin of that key, which -print-pin shows.
// Each flag can also come from the environment variable named in its help.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	relayprotocol "github.com/jermainee/homeport/relay-protocol"

	"github.com/jermainee/homeport/agent"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("homeport-agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	gateway := flags.String("gateway", os.Getenv("HOMEPORT_GATEWAY"), "gateway QUIC address as host:port (env HOMEPORT_GATEWAY)")
	gatewayPin := flags.String("gateway-pin", os.Getenv("HOMEPORT_GATEWAY_PIN"), "SPKI pin of the gateway's certificate (env HOMEPORT_GATEWAY_PIN)")
	keyPath := flags.String("key", envOr("HOMEPORT_AGENT_KEY", "homeport-agent.key"), "agent key file, created if missing (env HOMEPORT_AGENT_KEY)")
	printPin := flags.Bool("print-pin", false, "print the agent's pin and exit")
	verbose := flags.Bool("v", false, "log each flow's kind and status (never its destination)")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	key, created, err := loadOrCreateKey(*keyPath)
	if err != nil {
		log.Error("agent key", "error", err)
		return 1
	}
	cert, err := relayprotocol.NewSelfSigned(key, "homeport-agent")
	if err != nil {
		log.Error("agent certificate", "error", err)
		return 1
	}
	pin := relayprotocol.PinOf(cert.Leaf)
	if created {
		log.Info("created a new agent key", "path", *keyPath, "pin", pin)
	}
	if *printPin {
		_, _ = fmt.Fprintln(stdout, pin)
		return 0
	}

	if *gateway == "" || *gatewayPin == "" {
		_, _ = fmt.Fprintln(stderr, "homeport-agent: -gateway and -gateway-pin are required")
		flags.Usage()
		return 2
	}
	gwPin, err := relayprotocol.ParsePin(*gatewayPin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "homeport-agent: -gateway-pin: %v\n", err)
		return 2
	}
	a, err := agent.New(agent.Config{
		GatewayAddr: *gateway,
		GatewayPin:  gwPin,
		Certificate: cert,
		Logger:      log,
	})
	if err != nil {
		log.Error("agent config", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("sharing this line", "version", agent.Version, "pin", pin)
	if err := a.Run(ctx); err != nil {
		log.Error("agent stopped", "error", err)
		return 1
	}
	log.Info("stopped")
	return 0
}

// loadOrCreateKey reads the agent's Ed25519 key from path, a PEM PKCS #8
// file, or creates one there (readable by the owner only) if path does not
// exist. It reports whether it created the key.
func loadOrCreateKey(path string) (ed25519.PrivateKey, bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The path is the operator's own flag.
	if errors.Is(err, fs.ErrNotExist) {
		key, err := createKey(path)
		return key, err == nil, err
	}
	if err != nil {
		return nil, false, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, false, fmt.Errorf("%s holds no PEM PRIVATE KEY block", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, false, fmt.Errorf("%s holds a %T, want an Ed25519 key", path, parsed)
	}
	return key, false, nil
}

func createKey(path string) (ed25519.PrivateKey, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // The path is the operator's own flag.
	if err != nil {
		return nil, err
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		_ = f.Close()
		return nil, err
	}
	return key, f.Close()
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
