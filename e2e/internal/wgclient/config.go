package wgclient

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// privateKeyPlaceholder stands where the client puts its private key in
// the config_text the API hands out (contract operation
// createWireguardPeer), so the API never sees the key.
const privateKeyPlaceholder = "{{PRIVATE_KEY}}"

// WithPrivateKey returns the API's config_text with the client's private
// key in place of the placeholder, as the app does before it saves the
// file or shows the QR code.
func WithPrivateKey(configText string, privateKey [32]byte) string {
	return strings.Replace(configText, privateKeyPlaceholder, base64.StdEncoding.EncodeToString(privateKey[:]), 1)
}

// ParseConfig reads a wg-quick .conf file with one peer, such as the one
// the API hands out, into a client config.
//
// The client tunnels everything to its one peer, so AllowedIPs and
// PersistentKeepalive are read but not used. It uses the config's first
// IPv4 address and DNS server only: gateways have no IPv6 exit yet.
func ParseConfig(text string) (Config, error) {
	var cfg Config
	var section string
	var sawKey bool
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(line[1 : len(line)-1])
			if section != "interface" && section != "peer" {
				return Config{}, fmt.Errorf("wgclient: line %d: unknown section %s", n, line)
			}
			if section == "peer" && cfg.PeerPublicKey != "" {
				return Config{}, fmt.Errorf("wgclient: line %d: more than one [Peer]", n)
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Config{}, fmt.Errorf("wgclient: line %d: want key = value", n)
		}
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
		var err error
		switch section + "." + key {
		case "interface.privatekey":
			sawKey = true
			cfg.PrivateKey, err = parseKey(value)
		case "interface.address":
			cfg.Address, err = firstIPv4(value, true)
		case "interface.dns":
			cfg.DNS, err = firstIPv4(value, false)
		case "interface.mtu":
			cfg.MTU, err = strconv.Atoi(value)
			if err == nil && (cfg.MTU < 576 || cfg.MTU > 65535) {
				err = errors.New("out of range")
			}
		case "peer.publickey":
			if _, err = parseKey(value); err == nil {
				cfg.PeerPublicKey = value
			}
		case "peer.endpoint":
			if _, _, err = net.SplitHostPort(value); err == nil {
				cfg.Endpoint = value
			}
		case "peer.allowedips", "peer.persistentkeepalive":
		default:
			return Config{}, fmt.Errorf("wgclient: line %d: unknown key %q in [%s]", n, key, section)
		}
		if err != nil {
			return Config{}, fmt.Errorf("wgclient: line %d: %s: %w", n, key, err)
		}
	}
	switch {
	case !sawKey:
		return Config{}, errors.New("wgclient: no PrivateKey")
	case !cfg.Address.IsValid():
		return Config{}, errors.New("wgclient: no IPv4 Address")
	case cfg.PeerPublicKey == "":
		return Config{}, errors.New("wgclient: no [Peer] PublicKey")
	case cfg.Endpoint == "":
		return Config{}, errors.New("wgclient: no [Peer] Endpoint")
	}
	return cfg, nil
}

func parseKey(value string) ([32]byte, error) {
	if value == privateKeyPlaceholder {
		return [32]byte{}, errors.New("still the API's placeholder; put the client's key there (WithPrivateKey)")
	}
	b, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(b) != 32 {
		return [32]byte{}, errors.New("not 32 bytes of base64")
	}
	return [32]byte(b), nil
}

// firstIPv4 returns the first IPv4 address in a comma-separated list of
// addresses, or of prefixes when prefixes is set.
func firstIPv4(list string, prefixes bool) (netip.Addr, error) {
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		var addr netip.Addr
		var err error
		if prefixes {
			var p netip.Prefix
			p, err = netip.ParsePrefix(item)
			addr = p.Addr()
		} else {
			addr, err = netip.ParseAddr(item)
		}
		if err != nil {
			return netip.Addr{}, err
		}
		if addr.Is4() {
			return addr, nil
		}
	}
	return netip.Addr{}, nil
}
