module github.com/jermainee/homeport/e2e

go 1.27.4

replace (
	github.com/jermainee/homeport/agent => ../agent
	github.com/jermainee/homeport/gateway => ../gateway
	github.com/jermainee/homeport/relay-protocol => ../relay-protocol
)

require (
	github.com/jermainee/homeport/agent v0.0.0
	github.com/jermainee/homeport/gateway v0.0.0
	github.com/jermainee/homeport/relay-protocol v0.0.0
	golang.zx2c4.com/wireguard v0.0.0-20261024220952-b3bc704ef0ee
)

require (
	github.com/google/btree v1.1.2 // indirect
	github.com/quic-go/quic-go v0.65.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/crypto/x509roots/fallback v0.0.0-20261113174652-9ccaac6145fb // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/time v0.7.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
	gvisor.dev/gvisor v0.0.0-20250503011706-39ed1f5ac29c // indirect
)
