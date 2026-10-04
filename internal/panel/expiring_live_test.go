package panel

import (
	"testing"
	"time"

	"client2api/internal/livecfg"
)

// TestExpiringSoonPrefersTheLiveSnapshot pins the panel half of the reload:
// after startup, the live holder is the authoritative window.  The static
// option is only the fallback for embedders that never install a holder.
func TestExpiringSoonPrefersTheLiveSnapshot(t *testing.T) {
	p := &panel{opts: Options{
		ExpiringSoon: 10 * time.Hour,
		Live: livecfg.New(livecfg.Snapshot{
			ExpiringSoon: 90 * time.Minute,
		}),
	}}
	if got := p.expiringSoon(); got != 90*time.Minute {
		t.Fatalf("expiringSoon = %s, want the live 90m", got)
	}

	p.opts.Live = nil
	if got := p.expiringSoon(); got != 10*time.Hour {
		t.Fatalf("fallback expiringSoon = %s, want 10h", got)
	}
}
