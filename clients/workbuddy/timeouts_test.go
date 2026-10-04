package workbuddy

import (
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// TestWorkbuddyTimeoutResolution pins the reference's fallback rules for the
// `upstream.*_timeout_seconds` trio.  The rules live in one place (the config
// readers) precisely so the transport can be handed three concrete durations,
// and these are the cases the reference's normalize() encodes: a short-RPC
// ceiling defaulting to 120s, a first-byte deadline that follows that ceiling
// when it was not set, and an idle window defaulting to 300s while still
// honouring an explicit 0.
func TestWorkbuddyTimeoutResolution(t *testing.T) {
	ptr := func(n int) *int { return &n }

	tests := []struct {
		name       string
		cfg        config
		wantTotal  time.Duration
		wantHeader time.Duration
		wantIdle   time.Duration
	}{
		{
			name:       "nothing configured falls back to the reference defaults",
			cfg:        config{},
			wantTotal:  120 * time.Second,
			wantHeader: 120 * time.Second,
			wantIdle:   300 * time.Second,
		},
		{
			name:       "a short RPC ceiling drags the header deadline with it",
			cfg:        config{TimeoutSeconds: ptr(30)},
			wantTotal:  30 * time.Second,
			wantHeader: 30 * time.Second,
			wantIdle:   300 * time.Second,
		},
		{
			name:       "an explicit header deadline wins over the ceiling",
			cfg:        config{TimeoutSeconds: ptr(30), HeaderTimeoutSeconds: ptr(45)},
			wantTotal:  30 * time.Second,
			wantHeader: 45 * time.Second,
			wantIdle:   300 * time.Second,
		},
		{
			name:       "a header deadline without a ceiling still falls back to 120s total",
			cfg:        config{HeaderTimeoutSeconds: ptr(45)},
			wantTotal:  120 * time.Second,
			wantHeader: 45 * time.Second,
			wantIdle:   300 * time.Second,
		},
		{
			name:       "a non-positive ceiling is the absent case, not a zero deadline",
			cfg:        config{TimeoutSeconds: ptr(0), HeaderTimeoutSeconds: ptr(-1)},
			wantTotal:  120 * time.Second,
			wantHeader: 120 * time.Second,
			wantIdle:   300 * time.Second,
		},
		{
			name:       "an explicit zero idle switches the monitor off",
			cfg:        config{IdleTimeoutSeconds: ptr(0)},
			wantTotal:  120 * time.Second,
			wantHeader: 120 * time.Second,
			wantIdle:   0,
		},
		{
			name:       "a configured idle window is honoured as written",
			cfg:        config{IdleTimeoutSeconds: ptr(90)},
			wantTotal:  120 * time.Second,
			wantHeader: 120 * time.Second,
			wantIdle:   90 * time.Second,
		},
		{
			name:       "a negative idle disables the monitor rather than inverting it",
			cfg:        config{IdleTimeoutSeconds: ptr(-5)},
			wantTotal:  120 * time.Second,
			wantHeader: 120 * time.Second,
			wantIdle:   -5 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.totalTimeout(); got != tt.wantTotal {
				t.Errorf("totalTimeout() = %s, want %s", got, tt.wantTotal)
			}
			if got := tt.cfg.headerTimeout(); got != tt.wantHeader {
				t.Errorf("headerTimeout() = %s, want %s", got, tt.wantHeader)
			}
			if got := tt.cfg.idleTimeout(); got != tt.wantIdle {
				t.Errorf("idleTimeout() = %s, want %s", got, tt.wantIdle)
			}
		})
	}
}

// TestWorkbuddyNewAppliesTheTimeoutTrio proves the resolved durations reach the
// places that actually enforce them: the control-plane client's Timeout, the
// shared transport's ResponseHeaderTimeout (the field the chat path really waits
// on), and the idle monitor's window.  A config that parsed but never landed on
// the transport would pass every resolution test above and still let a hung
// upstream hold a request open forever.
func TestWorkbuddyNewAppliesTheTimeoutTrio(t *testing.T) {
	client, err := New(core.Deps{
		DataDir: t.TempDir(),
		Config:  []byte(`{"timeout_seconds":30,"header_timeout_seconds":45,"idle_timeout_seconds":7}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := client.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", client)
	}

	if got := c.up.HTTP.Timeout; got != 30*time.Second {
		t.Errorf("control-plane Timeout = %s, want 30s", got)
	}
	if got := c.up.HeaderTimeout; got != 45*time.Second {
		t.Errorf("HeaderTimeout = %s, want 45s", got)
	}
	if got := c.up.IdleTimeout; got != 7*time.Second {
		t.Errorf("IdleTimeout = %s, want 7s", got)
	}

	// The chat client carries no whole-request deadline: a long stream is
	// bounded by the idle monitor instead.
	if got := c.up.ChatHTTP.Timeout; got != 0 {
		t.Errorf("chat Timeout = %s, want 0", got)
	}

	chatTr, ok := c.up.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("chat transport is %T, want *http.Transport", c.up.ChatHTTP.Transport)
	}
	if got := chatTr.ResponseHeaderTimeout; got != 45*time.Second {
		t.Errorf("chat ResponseHeaderTimeout = %s, want 45s", got)
	}

	// The control plane shares that transport, so the same deadline covers a
	// refresh or a check-in that never answers.
	ctlTr, ok := c.up.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("control-plane transport is %T, want *http.Transport", c.up.HTTP.Transport)
	}
	if got := ctlTr.ResponseHeaderTimeout; got != 45*time.Second {
		t.Errorf("control-plane ResponseHeaderTimeout = %s, want 45s", got)
	}
}

// TestWorkbuddyNewAppliesTheReferenceFallbacks checks the same wiring through the
// default path: a config that names only the short-RPC ceiling must still leave
// the chat path with a first-byte deadline (the reference's header → total
// rule), and a config that says nothing at all must land on 120s/120s/300s.
func TestWorkbuddyNewAppliesTheReferenceFallbacks(t *testing.T) {
	tests := []struct {
		name       string
		config     string
		wantTotal  time.Duration
		wantHeader time.Duration
		wantIdle   time.Duration
	}{
		{
			name:       "only a ceiling",
			config:     `{"timeout_seconds":60}`,
			wantTotal:  60 * time.Second,
			wantHeader: 60 * time.Second,
			wantIdle:   300 * time.Second,
		},
		{
			name:       "nothing",
			config:     `{}`,
			wantTotal:  120 * time.Second,
			wantHeader: 120 * time.Second,
			wantIdle:   300 * time.Second,
		},
		{
			name:       "an explicit zero idle survives the wiring",
			config:     `{"idle_timeout_seconds":0}`,
			wantTotal:  120 * time.Second,
			wantHeader: 120 * time.Second,
			wantIdle:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := New(core.Deps{DataDir: t.TempDir(), Config: []byte(tt.config)})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			c, ok := client.(*Client)
			if !ok {
				t.Fatalf("New returned %T, want *Client", client)
			}
			if got := c.up.HTTP.Timeout; got != tt.wantTotal {
				t.Errorf("control-plane Timeout = %s, want %s", got, tt.wantTotal)
			}
			if got := c.up.HeaderTimeout; got != tt.wantHeader {
				t.Errorf("HeaderTimeout = %s, want %s", got, tt.wantHeader)
			}
			if got := c.up.IdleTimeout; got != tt.wantIdle {
				t.Errorf("IdleTimeout = %s, want %s", got, tt.wantIdle)
			}
			tr, ok := c.up.ChatHTTP.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("chat transport is %T, want *http.Transport", c.up.ChatHTTP.Transport)
			}
			if got := tr.ResponseHeaderTimeout; got != tt.wantHeader {
				t.Errorf("chat ResponseHeaderTimeout = %s, want %s", got, tt.wantHeader)
			}
		})
	}
}

// TestWorkbuddyNakedUpstreamKeepsTheTransportSafetyNet pins the constructor
// defaults for a caller that never wires the config file (a test, or a future
// caller).  The reference documents its transport constant as exactly this: a
// safety net that the configured value overrides, not the value a deployment
// runs on.  A zero here would mean an unconfigured client waits forever for
// response headers.
func TestWorkbuddyNakedUpstreamKeepsTheTransportSafetyNet(t *testing.T) {
	up := NewUpstream()
	if up.HTTP.Timeout != defaultHTTPTimeout {
		t.Errorf("HTTP.Timeout = %s, want %s", up.HTTP.Timeout, defaultHTTPTimeout)
	}
	if up.HeaderTimeout != responseHeaderTimeout {
		t.Errorf("HeaderTimeout = %s, want %s", up.HeaderTimeout, responseHeaderTimeout)
	}
	if up.IdleTimeout != defaultIdleTimeout {
		t.Errorf("IdleTimeout = %s, want %s", up.IdleTimeout, defaultIdleTimeout)
	}
	if responseHeaderTimeout <= 0 {
		t.Fatalf("the transport safety net is %s; an unconfigured client would hang", responseHeaderTimeout)
	}
	tr, ok := up.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("chat transport is %T, want *http.Transport", up.ChatHTTP.Transport)
	}
	if tr.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Errorf("chat ResponseHeaderTimeout = %s, want %s", tr.ResponseHeaderTimeout, responseHeaderTimeout)
	}
}

// TestWorkbuddySetTimeoutsLeavesUnsetFieldsAlone covers the setter's contract for
// a caller that only has one of the three values to apply: a non-positive total
// or header must not zero a live deadline, while idle is applied as given so the
// documented "monitor nothing" switch keeps working.
func TestWorkbuddySetTimeoutsLeavesUnsetFieldsAlone(t *testing.T) {
	up := NewUpstream()
	up.SetTimeouts(0, 0, 12*time.Second)

	if up.HTTP.Timeout != defaultHTTPTimeout {
		t.Errorf("a zero total overwrote the ceiling: %s", up.HTTP.Timeout)
	}
	if up.HeaderTimeout != responseHeaderTimeout {
		t.Errorf("a zero header overwrote the deadline: %s", up.HeaderTimeout)
	}
	tr, ok := up.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("chat transport is %T, want *http.Transport", up.ChatHTTP.Transport)
	}
	if tr.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Errorf("a zero header reached the transport: %s", tr.ResponseHeaderTimeout)
	}
	if up.IdleTimeout != 12*time.Second {
		t.Errorf("IdleTimeout = %s, want 12s", up.IdleTimeout)
	}

	// A nil receiver is the shape a Client assembled by a test can produce; it
	// must not panic, because New is not the only way to reach this method.
	var nilUp *Upstream
	nilUp.SetTimeouts(time.Second, time.Second, time.Second)
}
