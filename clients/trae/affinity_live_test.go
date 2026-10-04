package trae

import (
	"testing"
	"time"

	"client2api/internal/core"
)

// trae is a live-reloadable module now, and the only thing it reads from the
// shared live settings is the stickiness window.  testClient bypasses New, so
// the table is attached here the way New attaches it.
func TestApplyLiveMovesTheConversationStickinessWindow(t *testing.T) {
	c := testClient(t, nil, nil, nil)
	c.affinity = core.NewAffinity(0)

	if got := c.affinity.TTL(); got != core.DefaultAffinityTTL {
		t.Fatalf("window = %s by default, want %s", got, core.DefaultAffinityTTL)
	}

	c.ApplyLive(core.LiveSettings{AffinityTTL: 3 * time.Hour, AffinityGCInterval: 20 * time.Minute})
	if got := c.affinity.TTL(); got != 3*time.Hour {
		t.Fatalf("window = %s after a reload, want 3h", got)
	}
	if got := c.affinity.GCInterval(); got != 20*time.Minute {
		t.Fatalf("sweep = %s after a reload, want 20m", got)
	}

	// A reload that says nothing about stickiness must leave the window alone,
	// which is what the zero value means here.
	c.ApplyLive(core.LiveSettings{})
	if got := c.affinity.TTL(); got != 3*time.Hour {
		t.Fatalf("window = %s after a partial reload, want 3h", got)
	}
	if got := c.affinity.GCInterval(); got != 20*time.Minute {
		t.Fatalf("sweep = %s after a partial reload, want 20m", got)
	}

	// A module whose table was never built (or a nil client) must not panic:
	// the tables are nil-safe and so is this method.
	var bare Client
	bare.ApplyLive(core.LiveSettings{AffinityTTL: time.Minute})
	var nilClient *Client
	nilClient.ApplyLive(core.LiveSettings{AffinityTTL: time.Minute})
}
