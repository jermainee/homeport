package netwatch

import "testing"

func TestUtunAndIPsecInterfacesAreTunnels(t *testing.T) {
	for name, want := range map[string]bool{
		"utun0":     true,
		"utun12":    true,
		"ipsec0":    true,
		"en0":       false,
		"en1":       false,
		"lo0":       false,
		"bridge100": false,
		"awdl0":     false,
		"llw0":      false,
		"anpi0":     false,
	} {
		if got := isTunnelInterface(name); got != want {
			t.Errorf("isTunnelInterface(%q) = %v, want %v", name, got, want)
		}
	}
}
