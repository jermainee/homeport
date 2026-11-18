//go:build !darwin && !linux

package netwatch

// Check finds the home interface. Only macOS and Linux have a check so far;
// elsewhere it reports no VPN and no home, so the agent's sockets follow the
// system's routes.
func Check(string) (Route, error) { return Route{}, nil }

func isTunnelInterface(string) bool { return false }

func defaultRouteInterfaces() ([]string, error) { return nil, nil }

func bindSocket(int, string, Interface) error { return nil }
