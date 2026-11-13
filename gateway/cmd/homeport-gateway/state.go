package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jermainee/homeport/gateway"
	relayprotocol "github.com/jermainee/homeport/relay-protocol"
)

// state is what the gateway keeps across restarts. Peers' configs name its
// WireGuard key and agents pin its relay certificate's key, so both must
// survive a restart.
type state struct {
	wgKey gateway.Key
	cert  tls.Certificate
}

// loadState reads the gateway's keys from dir, creating them on first
// start. A file that exists but cannot be read is an error, never replaced.
func loadState(dir string) (state, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return state{}, fmt.Errorf("state dir: %w", err)
	}
	wgKey, err := loadWGKey(filepath.Join(dir, "wg.key"))
	if err != nil {
		return state{}, err
	}
	cert, err := loadRelayCert(filepath.Join(dir, "relay.crt"), filepath.Join(dir, "relay.key"))
	if err != nil {
		return state{}, err
	}
	return state{wgKey: wgKey, cert: cert}, nil
}

func loadWGKey(path string) (gateway.Key, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- a file in the operator's state dir
	if errors.Is(err, fs.ErrNotExist) {
		k, err := gateway.GenerateKey()
		if err != nil {
			return gateway.Key{}, err
		}
		if err := writeNew(path, []byte(k.String()+"\n"), 0o600); err != nil {
			return gateway.Key{}, err
		}
		return k, nil
	}
	if err != nil {
		return gateway.Key{}, fmt.Errorf("read WireGuard key: %w", err)
	}
	k, err := gateway.ParseKey(strings.TrimSpace(string(b)))
	if err != nil {
		return gateway.Key{}, fmt.Errorf("%s: %w", path, err)
	}
	return k, nil
}

func loadRelayCert(certPath, keyPath string) (tls.Certificate, error) {
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	if errors.Is(certErr, fs.ErrNotExist) && errors.Is(keyErr, fs.ErrNotExist) {
		cert, err := relayprotocol.GenerateSelfSigned("homeport-gateway")
		if err != nil {
			return tls.Certificate{}, err
		}
		certPEM, keyPEM, err := relayprotocol.MarshalPEM(cert)
		if err != nil {
			return tls.Certificate{}, err
		}
		if err := writeNew(keyPath, keyPEM, 0o600); err != nil {
			return tls.Certificate{}, err
		}
		if err := writeNew(certPath, certPEM, 0o644); err != nil {
			return tls.Certificate{}, err
		}
		return cert, nil
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("relay certificate: %w", err)
	}
	return cert, nil
}

// writeNew writes a file that must not exist yet, so two starts racing on
// an empty state dir cannot overwrite each other's keys.
func writeNew(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) // #nosec G304 -- a file in the operator's state dir
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
