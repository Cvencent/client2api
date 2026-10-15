package panel

import (
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/modelpark"
)

func statusAccount(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	entry := onlyClient(t, body)
	accounts, ok := entry["accounts"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("accounts = %v, want exactly one", entry["accounts"])
	}
	acct, ok := accounts[0].(map[string]any)
	if !ok {
		t.Fatalf("account = %T, want an object", accounts[0])
	}
	return acct
}

func accountParks(t *testing.T, acct map[string]any) []any {
	t.Helper()
	extra, ok := acct["extra"].(map[string]any)
	if !ok {
		t.Fatalf("extra = %v, want an object", acct["extra"])
	}
	raw, ok := extra["model_cooldowns"].([]any)
	if !ok {
		t.Fatalf("model_cooldowns = %v, want a list", extra["model_cooldowns"])
	}
	return raw
}

// TestStatusMergesGatewayParksIntoModelCooldowns pins the panel half of the
// gateway's platform-independent rate-limit memory: a module that publishes no
// per-model parks must still show the model as limited, with its release time.
func TestStatusMergesGatewayParksIntoModelCooldowns(t *testing.T) {
	parks := modelpark.New()
	parks.Park("cline", "acct-1", "deepseek-v4.1-flash", "429 slow down", 0)
	c := &fakeClient{
		name: "cline",
		status: core.Status{
			Ready: true,
			Accounts: []core.AccountStatus{{
				ID:      "acct-1",
				State:   "ready",
				Enabled: true,
			}},
		},
	}
	h := New(Options{Registry: registryOf(c), Started: time.Now(), ModelParks: parks})

	rec := get(t, h, "/panel/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	raw := accountParks(t, statusAccount(t, decodeMap(t, rec)))
	if len(raw) != 1 {
		t.Fatalf("model_cooldowns = %v, want one entry", raw)
	}
	park, ok := raw[0].(map[string]any)
	if !ok {
		t.Fatalf("park = %T, want an object", raw[0])
	}
	if park["model"] != "deepseek-v4.1-flash" {
		t.Fatalf("park model = %v, want deepseek-v4.1-flash", park["model"])
	}
	if park["kind"] != core.ModelParkRateLimit {
		t.Fatalf("park kind = %v, want %q", park["kind"], core.ModelParkRateLimit)
	}
	if until, _ := park["until"].(string); until == "" {
		t.Fatal("park has no release instant")
	}
}

// TestStatusKeepsAModulesOwnParkOverTheGateways pins the precedence rule: a
// module knows whether a refusal was a timed rate limit or a deterministic
// "this model is not available here", while the gateway only sees the 429.
func TestStatusKeepsAModulesOwnParkOverTheGateways(t *testing.T) {
	parks := modelpark.New()
	parks.Park("workbuddy", "acct-1", "m1", "429 slow down", 0)
	own := core.NewModelPark("m1", core.ModelParkUnsupported, "not available here", time.Now().Add(time.Hour), time.Time{})
	c := &fakeClient{
		name: "workbuddy",
		status: core.Status{
			Ready: true,
			Accounts: []core.AccountStatus{{
				ID:      "acct-1",
				State:   "ready",
				Enabled: true,
				Extra:   map[string]any{"model_cooldowns": []core.ModelPark{own}},
			}},
		},
	}
	h := New(Options{Registry: registryOf(c), Started: time.Now(), ModelParks: parks})

	raw := accountParks(t, statusAccount(t, decodeMap(t, get(t, h, "/panel/api/status"))))
	if len(raw) != 1 {
		t.Fatalf("model_cooldowns = %v, want the module's single entry", raw)
	}
	park, ok := raw[0].(map[string]any)
	if !ok {
		t.Fatalf("park = %T, want an object", raw[0])
	}
	if park["kind"] != core.ModelParkUnsupported {
		t.Fatalf("park kind = %v, want the module's %q", park["kind"], core.ModelParkUnsupported)
	}
}
