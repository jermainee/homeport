package wgclient

import (
	"encoding/base64"
	"encoding/json"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// examplePeer is the contract's example answer to createWireguardPeer.
func examplePeer(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../../contracts/examples/member/wireguard-peer-created.json")
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ConfigText string `json:"config_text"`
	}
	if err := json.Unmarshal(b, &created); err != nil {
		t.Fatal(err)
	}
	return created.ConfigText
}

func testKey() [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestTheAPIsConfigWithTheClientsKeyIsAClientConfig(t *testing.T) {
	text := WithPrivateKey(examplePeer(t), testKey())

	cfg, err := ParseConfig(text)

	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		PrivateKey:    testKey(),
		Address:       netip.MustParseAddr("10.77.0.2"),
		DNS:           netip.MustParseAddr("10.77.0.1"),
		Endpoint:      "fra1.gw.homeport.example:51820",
		PeerPublicKey: "YND9Jx2S347AP/Ywks3VrMciTOYivx8pQxgWoPy1HHY=",
	}
	if cfg != want {
		t.Errorf("got %+v\nwant %+v", cfg, want)
	}
}

func TestWithPrivateKeyPutsTheKeyWhereThePlaceholderStands(t *testing.T) {
	text := WithPrivateKey(examplePeer(t), testKey())

	line := "PrivateKey = " + base64.StdEncoding.EncodeToString(func() []byte { k := testKey(); return k[:] }())
	if !strings.Contains(text, line+"\n") || strings.Contains(text, "{{PRIVATE_KEY}}") {
		t.Errorf("config:\n%s", text)
	}
}

func TestParseConfigReadsTheMTU(t *testing.T) {
	text := "[Interface]\nPrivateKey = " + base64.StdEncoding.EncodeToString(make([]byte, 32)) +
		"\nAddress = 10.77.1.3/32, fd77::103/128\nDNS = 10.77.0.1\nMTU = 1280\n\n" +
		"[Peer]\nPublicKey = YND9Jx2S347AP/Ywks3VrMciTOYivx8pQxgWoPy1HHY=\nAllowedIPs = 0.0.0.0/0, ::/0\n" +
		"Endpoint = 127.0.0.1:51820\nPersistentKeepalive = 25\n"

	cfg, err := ParseConfig(text)

	if err != nil {
		t.Fatal(err)
	}
	if cfg.MTU != 1280 || cfg.Address != netip.MustParseAddr("10.77.1.3") || cfg.Endpoint != "127.0.0.1:51820" {
		t.Errorf("got %+v", cfg)
	}
}

func TestParseConfigRefusesAnIncompleteConfig(t *testing.T) {
	full := WithPrivateKey(examplePeer(t), testKey())
	for name, text := range map[string]string{
		"the private key placeholder": examplePeer(t),
		"no private key":              dropLine(full, "PrivateKey"),
		"no IPv4 address":             strings.Replace(full, "10.77.0.2/32, ", "", 1),
		"no peer public key":          dropLine(full, "PublicKey"),
		"no endpoint":                 dropLine(full, "Endpoint"),
		"a malformed MTU":             strings.Replace(full, "DNS = 10.77.0.1", "DNS = 10.77.0.1\nMTU = big", 1),
		"an unknown key":              strings.Replace(full, "DNS = 10.77.0.1", "DNS = 10.77.0.1\nTable = off", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if cfg, err := ParseConfig(text); err == nil {
				t.Errorf("accepted %+v", cfg)
			}
		})
	}
}

func dropLine(text, key string) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if !strings.HasPrefix(l, key+" ") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
