// Command wgcurl fetches a URL through a WireGuard tunnel in userspace, as
// a member's device would through a Homeport gateway. It changes no routes
// and needs no root, so it runs on the same machine as the sharing agent.
//
// Usage:
//
//	wgcurl -conf member.conf [-o FILE] URL
//
// The .conf file is a wg-quick config with the private key filled in, such
// as the one `member peer` prints. Names resolve through the tunnel's DNS,
// that is the sharing agent's resolver. The body goes to stdout (or -o);
// the status, the body's size and SHA-256, and the inner bytes the tunnel
// carried go to stderr, so they can be checked against the gateway's meter.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/jermainee/homeport/e2e/internal/wgclient"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("wgcurl", flag.ContinueOnError)
	flags.SetOutput(stderr)
	confPath := flags.String("conf", "", "wg-quick config with the private key filled in")
	outPath := flags.String("o", "", "write the body to this file instead of stdout")
	timeout := flags.Duration("timeout", 2*time.Minute, "give up after this long")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *confPath == "" || flags.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: wgcurl -conf member.conf [-o FILE] URL")
		return 2
	}
	text, err := os.ReadFile(*confPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wgcurl: %v\n", err)
		return 1
	}
	cfg, err := wgclient.ParseConfig(string(text))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wgcurl: %s: %v\n", *confPath, err)
		return 1
	}
	out := stdout
	var outFile *os.File
	if *outPath != "" {
		if outFile, err = os.Create(*outPath); err != nil {
			_, _ = fmt.Fprintf(stderr, "wgcurl: %v\n", err)
			return 1
		}
		defer func() { _ = outFile.Close() }() // a second Close only returns an error
		out = outFile
	}

	client, err := wgclient.New(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wgcurl: %v\n", err)
		return 1
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	code := fetch(ctx, client, flags.Arg(0), out, stderr)
	if outFile != nil {
		// A write the OS deferred can still fail here, as on a full disk.
		if err := outFile.Close(); err != nil {
			_, _ = fmt.Fprintf(stderr, "wgcurl: %v\n", err)
			code = 1
		}
	}
	sent, received := settle(client)
	_, _ = fmt.Fprintf(stderr, "wgcurl: tunnel carried %d bytes: %d sent, %d received\n", sent+received, sent, received)
	return code
}

func fetch(ctx context.Context, client *wgclient.Client, url string, out, stderr io.Writer) int {
	hc := &http.Client{Transport: &http.Transport{DialContext: client.Net.DialContext, DisableKeepAlives: true}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wgcurl: %v\n", err)
		return 2
	}
	resp, err := hc.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wgcurl: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), resp.Body)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wgcurl: reading the body: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "wgcurl: %s, %d bytes, sha256 %s\n", resp.Status, n, hex.EncodeToString(h.Sum(nil)))
	if resp.StatusCode >= 400 {
		return 1
	}
	return 0
}

// settle waits until the tunnel's counters stop moving (the last ACKs and
// FINs), for up to 5 s, and returns them.
func settle(client *wgclient.Client) (sent, received uint64) {
	deadline := time.Now().Add(5 * time.Second)
	sent, received = client.Sent(), client.Received()
	stableSince := time.Now()
	for time.Now().Before(deadline) && time.Since(stableSince) < time.Second {
		time.Sleep(50 * time.Millisecond)
		if s, r := client.Sent(), client.Received(); s != sent || r != received {
			sent, received, stableSince = s, r, time.Now()
		}
	}
	return sent, received
}
