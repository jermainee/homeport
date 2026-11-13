// Package service runs a gateway from the control API, the way production
// runs it: it announces the gateway, keeps its peers and lines in sync with
// the API's config, holds a quota lease for each peer's session, reports
// usage and line events, and shuts down in order. cmd/homeport-gateway is a
// thin wrapper around it, and the end-to-end tests in e2e/ run it the same
// way.
package service

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jermainee/homeport/gateway"
	"github.com/jermainee/homeport/gateway/internal/controlplane"
)

var _ controlplane.Gateway = (*gateway.Server)(nil)

const (
	announceRetry   = 10 * time.Second // between announcements while the API is unreachable
	shutdownTimeout = 15 * time.Second // for the final usage, lease returns and line events
)

// Config configures a Service.
type Config struct {
	API   string // control API base URL, such as https://api.homeport.example
	Token string // gateway token, hpgw_<id>.<secret>

	PrivateKey   gateway.Key // WireGuard key
	WGListenPort uint16      // UDP port WireGuard listens on; 0 picks a free one
	// WGEndpoint is the public host:port members' WireGuard configs
	// connect to.
	WGEndpoint string

	// QUICListen is the UDP address sharing agents dial, such as ":4433".
	QUICListen string
	// QUICEndpoint is the public host:port agents connect to. Empty means
	// the WGEndpoint host with the port QUICListen got.
	QUICEndpoint string
	// Certificate is the relay certificate; agents pin its key.
	Certificate tls.Certificate

	Version string       // the gateway's release, sent to the API
	Logger  *slog.Logger // nil means slog.Default()
}

// Service is a gateway run from the control API.
type Service struct {
	cfg  Config
	log  *slog.Logger
	api  *controlplane.Client
	ctl  *controlplane.Controller
	srv  *gateway.Server
	self controlplane.GatewaySelf
}

// New starts the gateway's WireGuard and QUIC listeners. It serves no peer
// and no agent until Run has fetched the config.
func New(cfg Config) (*Service, error) {
	if cfg.API == "" || cfg.Token == "" {
		return nil, errors.New("service: Config.API and Config.Token are required")
	}
	wgHost, _, err := net.SplitHostPort(cfg.WGEndpoint)
	if err != nil {
		return nil, fmt.Errorf("service: WireGuard endpoint: %w", err)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	api := controlplane.NewClient(cfg.API, cfg.Token)
	ctl := controlplane.New(api, controlplane.Options{Logger: log})
	srv, err := gateway.New(gateway.Config{
		PrivateKey:  cfg.PrivateKey,
		ListenPort:  cfg.WGListenPort,
		QUICListen:  cfg.QUICListen,
		Certificate: cfg.Certificate,
		Access:      ctl,
		OnLineEvent: ctl.OnLineEvent,
	})
	if err != nil {
		return nil, err
	}
	ctl.Attach(srv)

	quicEndpoint := cfg.QUICEndpoint
	if quicEndpoint == "" {
		_, port, _ := net.SplitHostPort(srv.QUICAddr().String())
		quicEndpoint = net.JoinHostPort(wgHost, port)
	}
	quicHost, _, err := net.SplitHostPort(quicEndpoint)
	if err != nil {
		_ = srv.Close()
		return nil, fmt.Errorf("service: QUIC endpoint: %w", err)
	}
	pin := srv.RelayPin()
	return &Service{
		cfg: cfg,
		log: log,
		api: api,
		ctl: ctl,
		srv: srv,
		self: controlplane.GatewaySelf{
			WGPublicKey:    srv.PublicKey().String(),
			WGEndpoint:     cfg.WGEndpoint,
			QUICEndpoint:   quicEndpoint,
			QUICServerName: quicHost,
			QUICSPKISHA256: hex.EncodeToString(pin[:]), // the contract wants hex; relay-protocol prints pins in base64
			Version:        cfg.Version,
		},
	}, nil
}

// Server is the running gateway: its listen addresses, keys and meter.
// The service drives its peers and lines; callers only read from it.
func (s *Service) Server() *gateway.Server { return s.srv }

// Run announces the gateway (PUT /v1/gateway/self), retrying while the API
// is unreachable, then serves the API's config until ctx ends. Then it shuts
// down in order: it ends every session (final usage, lease returns), closes
// the gateway, which disconnects its agents, and reports those lines
// offline. It returns an error, with the gateway closed, if the API refuses
// the announcement or ctx ends before the API accepts it.
func (s *Service) Run(ctx context.Context) error {
	if err := s.announce(ctx); err != nil {
		_ = s.srv.Close()
		return err
	}
	s.log.Info("gateway running", "version", s.cfg.Version, "wg_port", s.srv.ListenPort(), "quic", s.srv.QUICAddr().String())
	s.ctl.Run(ctx)

	shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	s.ctl.Close(shutdown)
	_ = s.srv.Close()
	s.ctl.Flush(shutdown)
	s.log.Info("gateway stopped")
	return nil
}

// announce sends PUT /v1/gateway/self, retrying while the API is
// unreachable or failing. A refusal (such as a wrong token) is fatal.
func (s *Service) announce(ctx context.Context) error {
	for {
		err := s.api.PutSelf(ctx, s.self)
		if err == nil {
			return nil
		}
		// A wrong token will not fix itself while the gateway waits.
		var ae *controlplane.APIError
		if !controlplane.Temporary(err) || (errors.As(err, &ae) && ae.Status == http.StatusUnauthorized) {
			return fmt.Errorf("announce the gateway: %w", err)
		}
		s.log.Warn("announce the gateway failed; will retry", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(announceRetry):
		}
	}
}
