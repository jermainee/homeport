// Command homeport-gateway runs Homeport's edge gateway from the control
// API: it terminates members' WireGuard tunnels, relays their flows to
// sharing agents over QUIC, and meters and reports every byte.
//
// The gateway token is read from HOMEPORT_GATEWAY_TOKEN.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jermainee/homeport/gateway"
	"github.com/jermainee/homeport/gateway/internal/controlplane"
)

// version is the gateway's release version, set at build time with
// -ldflags "-X main.version=...".
var version = "0.1.0"

var _ controlplane.Gateway = (*gateway.Server)(nil)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Error("gateway stopped", "err", err)
		os.Exit(1)
	}
}

type options struct {
	api          string
	wgListen     string
	wgEndpoint   string
	quicListen   string
	quicEndpoint string
	stateDir     string
}

func parseFlags(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("homeport-gateway", flag.ContinueOnError)
	fs.StringVar(&o.api, "api", "", "control API base URL, such as https://api.homeport.example")
	fs.StringVar(&o.wgListen, "wg-listen", ":51820", "UDP address WireGuard listens on (all interfaces; only the port is used)")
	fs.StringVar(&o.wgEndpoint, "wg-endpoint", "", "public host:port members' WireGuard configs connect to")
	fs.StringVar(&o.quicListen, "quic-listen", ":4433", "UDP address sharing agents connect to")
	fs.StringVar(&o.quicEndpoint, "quic-endpoint", "", "public host:port agents connect to (default: the --wg-endpoint host with the --quic-listen port)")
	fs.StringVar(&o.stateDir, "state-dir", "/var/lib/homeport-gateway", "directory for the gateway's WireGuard key and relay certificate")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if o.api == "" || o.wgEndpoint == "" {
		return options{}, errors.New("--api and --wg-endpoint are required")
	}
	if _, _, err := net.SplitHostPort(o.wgEndpoint); err != nil {
		return options{}, fmt.Errorf("--wg-endpoint: %w", err)
	}
	if o.quicEndpoint == "" {
		host, _, _ := net.SplitHostPort(o.wgEndpoint)
		_, port, err := net.SplitHostPort(o.quicListen)
		if err != nil {
			return options{}, fmt.Errorf("--quic-listen: %w", err)
		}
		o.quicEndpoint = net.JoinHostPort(host, port)
	}
	return o, nil
}

func run(log *slog.Logger, args []string) error {
	o, err := parseFlags(args)
	if err != nil {
		return err
	}
	token := os.Getenv("HOMEPORT_GATEWAY_TOKEN")
	if !strings.HasPrefix(token, "hpgw_") {
		return errors.New("HOMEPORT_GATEWAY_TOKEN must hold the gateway token (hpgw_<id>.<secret>)")
	}
	wgPort, err := listenPort(o.wgListen)
	if err != nil {
		return fmt.Errorf("--wg-listen: %w", err)
	}
	st, err := loadState(o.stateDir)
	if err != nil {
		return err
	}

	api := controlplane.NewClient(o.api, token)
	ctl := controlplane.New(api, controlplane.Options{Logger: log})
	srv, err := gateway.New(gateway.Config{
		PrivateKey:  st.wgKey,
		ListenPort:  wgPort,
		QUICListen:  o.quicListen,
		Certificate: st.cert,
		Access:      ctl,
		OnLineEvent: ctl.OnLineEvent,
	})
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()
	ctl.Attach(srv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pin := srv.RelayPin()
	quicHost, _, _ := net.SplitHostPort(o.quicEndpoint)
	self := controlplane.GatewaySelf{
		WGPublicKey:    srv.PublicKey().String(),
		WGEndpoint:     o.wgEndpoint,
		QUICEndpoint:   o.quicEndpoint,
		QUICServerName: quicHost,
		QUICSPKISHA256: hex.EncodeToString(pin[:]), // the contract wants hex; relay-protocol prints pins in base64
		Version:        version,
	}
	if err := announce(ctx, log, api, self); err != nil {
		return err
	}
	log.Info("gateway running", "version", version, "wg_port", srv.ListenPort(), "quic", o.quicListen)
	ctl.Run(ctx)
	log.Info("gateway stopped")
	return nil
}

// announce sends PUT /v1/gateway/self, retrying while the API is
// unreachable or failing. A refusal (such as a wrong token) is fatal.
func announce(ctx context.Context, log *slog.Logger, api *controlplane.Client, self controlplane.GatewaySelf) error {
	for {
		err := api.PutSelf(ctx, self)
		var ae *controlplane.APIError
		if err == nil || !errors.As(err, &ae) || (ae.Status != 0 && ae.Status < 500 && ae.Status != http.StatusTooManyRequests) {
			if err != nil {
				return fmt.Errorf("announce the gateway: %w", err)
			}
			return nil
		}
		log.Warn("announce the gateway failed; will retry", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// listenPort returns the port of a listen address. WireGuard listens on
// every interface, so a host other than a wildcard is refused.
func listenPort(addr string) (uint16, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return 0, fmt.Errorf("WireGuard listens on all interfaces; give only the port, as :%s", port)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("port %q: %w", port, err)
	}
	return uint16(p), nil
}
