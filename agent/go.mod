module github.com/jermainee/homeport/agent

go 1.27.4

require (
	github.com/jermainee/homeport/relay-protocol v0.0.0
	github.com/quic-go/quic-go v0.65.0
	golang.org/x/crypto/x509roots/fallback v0.0.0-20261113174652-9ccaac6145fb
	golang.org/x/net v0.56.0
)

require (
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/jermainee/homeport/relay-protocol => ../relay-protocol
