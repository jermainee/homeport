//go:build !darwin

package netwatch

// Tunneled reports whether another VPN carries the default route. Only macOS
// has a check so far; elsewhere it always reports false.
func Tunneled() (bool, error) { return false, nil }
