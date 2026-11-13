module github.com/jermainee/homeport/desktop-app

go 1.27.4

require (
	github.com/coder/websocket v1.8.14 // indirect
	github.com/ebitengine/purego v0.11.1 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/quic-go/quic-go v0.65.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/term v0.46.0 // indirect
)

require (
	github.com/jermainee/homeport/agent v0.0.0
	github.com/jermainee/homeport/relay-protocol v0.0.0
	github.com/wailsapp/wails/v3 v3.1.0
)

// Replace directives are not transitive, so the app names relay-protocol too.
replace (
	github.com/jermainee/homeport/agent => ../agent
	github.com/jermainee/homeport/relay-protocol => ../relay-protocol
)
