package panel

import (
	"strings"
	"testing"
)

// A listen address the process cannot bind used to save fine: the toast then
// pointed at the restart button, the fresh process died on net.Listen, and the
// page kept saying "connected" (issue #19).  validateConfig is the gate.
func TestValidateConfigChecksTheListenAddress(t *testing.T) {
	good := []string{"", "127.0.0.1:8788", ":8788", "[::1]:8788", "0.0.0.0:80", "gateway.lan:443",
		// Port 0 means the OS picks one -- the test-suite convention, and a
		// legal bind even though the panel address then moves on restart.
		"127.0.0.1:0"}
	for _, addr := range good {
		if err := validateConfig(map[string]any{"listen": addr}); err != nil {
			t.Errorf("validateConfig(listen %q) = %v, want accepted", addr, err)
		}
	}
	bad := map[string]string{
		"127.0.0.1":       "missing port",
		"garbage":         "missing port",
		"127.0.0.1:99999": "between 0 and 65535",
		"127.0.0.1:abc":   "between 0 and 65535",
		" 127.0.0.1:8788": "whitespace",
	}
	for addr, want := range bad {
		err := validateConfig(map[string]any{"listen": addr})
		if err == nil {
			t.Errorf("validateConfig(listen %q) = nil, want rejected", addr)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validateConfig(listen %q) = %q, want it to mention %q", addr, err.Error(), want)
		}
	}
	// Absent and nil stay legal: applyDefaults fills the built-in address.
	if err := validateConfig(map[string]any{}); err != nil {
		t.Errorf("validateConfig(no listen) = %v, want accepted", err)
	}
	if err := validateConfig(map[string]any{"listen": nil}); err != nil {
		t.Errorf("validateConfig(listen nil) = %v, want accepted", err)
	}
}
