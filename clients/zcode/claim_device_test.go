package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The claim path has two behaviours that only exist because the vendor's
// interface is not what it was: the captcha is no longer required, and a 3012
// risk-control refusal is about the device id rather than the credential.
// Both were established by measurement against the live vendor on 2026-10-28
// and both are easy to undo by accident, so they are pinned here.

// writeDesktopDeviceMid plants a telemetry-state.json the way the ZCode desktop
// client writes it, so readDesktopDeviceMid can find it under the isolated home.
func writeDesktopDeviceMid(t *testing.T, home, mid string) {
	t.Helper()
	dir := filepath.Join(home, ".zcode", "v2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"deviceMid": mid})
	if err := os.WriteFile(filepath.Join(dir, "telemetry-state.json"), body, 0o600); err != nil {
		t.Fatalf("write telemetry-state.json: %v", err)
	}
}

func TestReadDesktopDeviceMidReadsTheClientsOwnFile(t *testing.T) {
	home := isolateHome(t)
	writeDesktopDeviceMid(t, home, "13208e45-0088-458f-bc4c-2eedddc2bb30")
	if got := readDesktopDeviceMid(); got != "13208e45-0088-458f-bc4c-2eedddc2bb30" {
		t.Errorf("readDesktopDeviceMid() = %q, want the client's id", got)
	}
	// A machine without the client must answer "" rather than error, because
	// that is the path that keeps the generated id in use.
	other := t.TempDir()
	t.Setenv("USERPROFILE", other)
	t.Setenv("HOME", other)
	if got := readDesktopDeviceMid(); got != "" {
		t.Errorf("readDesktopDeviceMid() without a client = %q, want empty", got)
	}
}

// TestPoolPrefersTheDesktopDeviceMid is the point of reading that file: the
// vendor's risk check keys on the device id, and the desktop client's is the
// one this machine already uses for everything else.
func TestPoolPrefersTheDesktopDeviceMid(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	const desktop = "13208e45-0088-458f-bc4c-2eedddc2bb30"
	writeDesktopDeviceMid(t, env.home, desktop)
	c := env.client(t, routeTransport(t, nil))
	if got := c.pool.deviceMid(); got != desktop {
		t.Errorf("pool device id = %q, want the desktop client's %q", got, desktop)
	}
}

// TestClaimRetriesAsTheDesktopDeviceAfterRiskControl drives the recovery: the
// first attempt is refused 3012, the pool adopts the desktop id, and the retry
// is what actually lands the plan.
func TestClaimRetriesAsTheDesktopDeviceAfterRiskControl(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	// The pool adopts the desktop id at construction, so this test needs the
	// recovery to be observable: the first claim is sent as a *different*
	// generated id, and the desktop file holds the one that succeeds.
	const desktop = "13208e45-0088-458f-bc4c-2eedddc2bb30"
	const generated = "9984a2c3-cbbf-456b-99ca-5c1d8ebf3a5c"

	var claimMids []string
	ft := routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimPreviewFixture), nil
		},
		planClaimPath: func(r *http.Request) (*http.Response, error) {
			mid := r.Header.Get("X-Device-Mid")
			claimMids = append(claimMids, mid)
			// The generated id is refused the way the live vendor refused ours;
			// the desktop id is accepted.
			if mid != desktop {
				return jsonResponse(http.StatusOK,
					`{"code":3012,"msg":"request has been blocked due to unusual activity."}`), nil
			}
			return jsonResponse(http.StatusOK,
				`{"code":0,"msg":"","data":{"plan":{"starts_at":1790692703,"ends_at":1790697600}}}`), nil
		},
	})
	// The pool resolves its device id at construction: config first, then the
	// client's file, then a generated one.  This test is about the recovery, so
	// pin the pool to a non-desktop id locally and let the desktop file appear
	// only afterwards -- otherwise loadDeviceLocked would have adopted it at
	// construction and there would be nothing left to switch to.
	c := env.client(t, ft)
	// Touch the pool once so it resolves its identity (which is what loads the
	// generated id and marks the pool ready), then pin it to the generated one
	// and only afterwards give the desktop client a file to be adopted from.
	_ = c.pool.deviceMid()
	c.pool.mu.Lock()
	loaded := c.pool.loaded
	c.pool.deviceID = generated
	c.pool.mu.Unlock()
	if !loaded {
		t.Fatal("the pool was not loaded after a device read; the identity would be re-resolved")
	}
	writeDesktopDeviceMid(t, env.home, desktop)

	if got := c.pool.deviceMid(); got != generated {
		t.Fatalf("pool device id = %q, want the pinned %q", got, generated)
	}

	id := addJWTAccount(t, c)
	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("res = %+v, want the retry as the desktop id to succeed", res)
	}
	if len(claimMids) != 2 {
		t.Fatalf("claim attempts = %d (%v), want exactly two", len(claimMids), claimMids)
	}
	if claimMids[0] != generated {
		t.Fatalf("first attempt used %q, want the generated id %q", claimMids[0], generated)
	}
	if claimMids[1] != desktop {
		t.Errorf("second attempt used %q, want the desktop id %q", claimMids[1], desktop)
	}
	// And the new identity is the one this process keeps using.
	if got := c.pool.deviceMid(); got != desktop {
		t.Errorf("pool device id after recovery = %q, want %q", got, desktop)
	}
}

// TestClaimDoesNotLoopOnRiskControlWithoutADesktopClient pins the other half:
// with no client installed there is nothing to adopt, so the refusal is
// reported as-is after one attempt rather than retried pointlessly.
func TestClaimDoesNotLoopOnRiskControlWithoutADesktopClient(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	attempts := 0
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimPreviewFixture), nil
		},
		planClaimPath: func(*http.Request) (*http.Response, error) {
			attempts++
			return jsonResponse(http.StatusOK,
				`{"code":3012,"msg":"request has been blocked due to unusual activity."}`), nil
		},
	}))
	id := addJWTAccount(t, c)

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatalf("res = %+v, want the refusal to stand", res)
	}
	if res.Code != codeRiskControl {
		t.Errorf("code = %d, want %d relayed", res.Code, codeRiskControl)
	}
	if attempts != 1 {
		t.Errorf("claim attempts = %d, want 1: there is no other identity to try", attempts)
	}
	if !strings.Contains(strings.ToLower(res.Message), "unusual activity") {
		t.Errorf("message = %q, want the vendor's own words", res.Message)
	}
}
