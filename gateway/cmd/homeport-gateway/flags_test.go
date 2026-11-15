package main

import (
	"net/netip"
	"testing"
	"time"

	"github.com/jermainee/homeport/gateway"
)

func TestCanaryFlags(t *testing.T) {
	base := []string{"--api", "https://api.homeport.example", "--wg-endpoint", "gw.homeport.example:51820"}

	o, err := parseFlags(base)
	if err != nil || o.canary != nil {
		t.Fatalf("without --canary-target: canary %+v, %v; want none", o.canary, err)
	}

	o, err = parseFlags(append(base, "--canary-target", "11.22.33.44:80", "--canary-dns-zone", "canary.homeport.example", "--canary-interval", "10m"))
	if err != nil {
		t.Fatal(err)
	}
	want := gateway.CanaryConfig{HTTPListen: ":80", HTTPTarget: netip.MustParseAddrPort("11.22.33.44:80"), DNSZone: "canary.homeport.example", Interval: 10 * time.Minute}
	if o.canary == nil || *o.canary != want {
		t.Errorf("canary = %+v, want %+v", o.canary, want)
	}

	for _, bad := range [][]string{
		{"--canary-target", "10.0.0.1:80"},   // agents refuse private destinations
		{"--canary-target", "gw.example:80"}, // must be an IP: the check dials it through the line
		{"--canary-dns-zone", "canary.homeport.example"},
		{"--canary-target", "11.22.33.44:80", "--canary-interval", "0s"},
	} {
		if _, err := parseFlags(append(base, bad...)); err == nil {
			t.Errorf("parseFlags(%v) succeeded", bad)
		}
	}
}
