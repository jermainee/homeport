module github.com/jermainee/homeport/gateway

go 1.27.4

require (
	github.com/jermainee/homeport/relay-protocol v0.0.0
	github.com/quic-go/quic-go v0.65.0
	golang.org/x/crypto v0.54.0
	golang.zx2c4.com/wireguard v0.0.0-20261024220952-b3bc704ef0ee
	gvisor.dev/gvisor v0.0.0-20250503011706-39ed1f5ac29c
)

require (
	github.com/google/btree v1.1.2 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/time v0.7.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
)

replace github.com/jermainee/homeport/relay-protocol => ../relay-protocol
