//go:build !windows

package tray

// There is no notification area to own an icon in.  The gateway also runs in
// the container image and on the CI matrix, where a tray would be meaningless,
// so Start simply reports that it has nothing to show and every caller's Stop
// stays a no-op.
func start(Options) (backend, error) { return backend{}, nil }
