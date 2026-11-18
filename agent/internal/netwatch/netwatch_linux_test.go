package netwatch

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDefaultRoutesFromProcNetRoute(t *testing.T) {
	const table = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	0002A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
wlan0	00000000	0102A8C0	0003	0	0	600	00000000	0	0	0
eth0	00000000	0102A8C0	0003	0	0	100	00000000	0	0	0
tun0	00000080	00000000	0001	0	0	0	00000080	0	0	0
eth1	00000000	0101A8C0	0002	0	0	50	00000000	0	0	0
`
	got := parseProcNetRoute(strings.NewReader(table))
	// By metric; tun0's 128.0.0.0/1 is not a default route, and eth1's
	// route is down.
	if want := []string{"eth0", "wlan0"}; !slices.Equal(got, want) {
		t.Fatalf("parseProcNetRoute = %v, want %v", got, want)
	}
}

func TestDefaultRoutesFromProcNetIPv6Route(t *testing.T) {
	const table = `20010db8000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001 eth0
00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003 eth0
00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200 lo
00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 00000400 00000001 00000000 00000001 wg0
`
	got := parseProcNetIPv6Route(strings.NewReader(table))
	// lo's is an unreachable route, and only /0 counts.
	if want := []string{"eth0", "wg0"}; !slices.Equal(got, want) {
		t.Fatalf("parseProcNetIPv6Route = %v, want %v", got, want)
	}
}

func TestWireGuardAndTunDevicesAreTunnels(t *testing.T) {
	// As in sysfs, /sys/class/net/<name> is a symlink to the device's
	// directory under /sys/devices.
	sys := t.TempDir()
	root := filepath.Join(sys, "class", "net")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	dev := func(name, typ string, tunFlags bool) {
		dir := filepath.Join(sys, "devices", "virtual", "net", name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", "..", "devices", "virtual", "net", name), filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "type"), []byte(typ+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if tunFlags {
			if err := os.WriteFile(filepath.Join(dir, "tun_flags"), []byte("0x1002\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	dev("wg0", "65534", false) // WireGuard: no link layer
	dev("tun0", "65534", true) // OpenVPN tun
	dev("tap0", "1", true)     // OpenVPN tap: Ethernet, but a tun device
	dev("eth0", "1", false)    // Ethernet
	dev("ppp0", "512", false)  // PPPoE: a DSL home line, not a VPN
	dev("wlan0", "1", false)   // Wi-Fi
	sysClassNet = root
	t.Cleanup(func() { sysClassNet = "/sys/class/net" })

	for name, want := range map[string]bool{
		"wg0": true, "tun0": true, "tap0": true,
		"eth0": false, "ppp0": false, "wlan0": false, "missing0": false,
		"../net/wg0": false, "": false,
	} {
		if got := isTunnelInterface(name); got != want {
			t.Errorf("isTunnelInterface(%q) = %v, want %v", name, got, want)
		}
	}
}
