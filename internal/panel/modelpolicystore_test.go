package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/modelpolicy"
)

// modelpolicystore_test.go pins the shared model policy through the panel
// surface: capability bit, account-list backfill, POST save and read-back.
// Testing only the capability bit would let the checkbox render while the save
// path goes nowhere.

func mustPolicyStore(t *testing.T) *modelpolicy.Store {
	t.Helper()
	s, err := modelpolicy.Open("")
	if err != nil {
		t.Fatalf("modelpolicy.Open: %v", err)
	}
	return s
}

// policyCopyClient behaves like a real module: every Accounts call returns a
// fresh record, so the panel's decoration cannot leak into the module's own
// state between requests.
type policyCopyClient struct {
	*fakeClient
	records []core.AccountRecord
}

func (c *policyCopyClient) AccountFields(context.Context) []core.FieldSpec { return nil }

func (c *policyCopyClient) Accounts(context.Context) ([]core.AccountRecord, error) {
	out := make([]core.AccountRecord, len(c.records))
	for i, r := range c.records {
		out[i] = r
		if r.Fields != nil {
			out[i].Fields = map[string]any{}
			for k, v := range r.Fields {
				out[i].Fields[k] = v
			}
		}
	}
	return out, nil
}

func (c *policyCopyClient) AddAccount(context.Context, core.AccountSpec) (core.AccountRecord, error) {
	return core.AccountRecord{}, nil
}
func (c *policyCopyClient) RemoveAccount(context.Context, string) error           { return nil }
func (c *policyCopyClient) SetAccountEnabled(context.Context, string, bool) error { return nil }
func (c *policyCopyClient) TestAccount(context.Context, string) (core.TestResult, error) {
	return core.TestResult{}, nil
}
func (c *policyCopyClient) RefreshAccount(context.Context, string) ([]core.RefreshResult, error) {
	return nil, nil
}

func plainPolicyClient(name, id string) *policyCopyClient {
	return &policyCopyClient{
		fakeClient: &fakeClient{name: name},
		records:    []core.AccountRecord{{ID: id}},
	}
}

func TestPanelAdvertisesFallbackModelPolicyOnlyWhenWired(t *testing.T) {
	store := mustPolicyStore(t)
	plain := plainPolicyClient("trae", "acct-1")
	p := &panel{opts: Options{Registry: registryOf(plain), ModelPolicies: store}}
	if caps := p.capabilities(context.Background(), plain); !caps.ModelPolicyStore {
		t.Error("plain module did not get model_policy_store though the deployment wired a shared store")
	}

	// A module with its own policy owns the blacklist itself; the fallback flag
	// must stay off so the panel does not claim two owners for one account.
	owned := modelPolicyFor("wb", core.AccountRecord{ID: "acct-1"})
	if caps := p.capabilities(context.Background(), owned); !caps.ModelPolicy || caps.ModelPolicyStore {
		t.Errorf("own-policy module caps = %+v, want ModelPolicy only", caps)
	}

	// No store at all: the fallback is not offered, so the dialog stays read-only
	// instead of sending a save that would 501.
	noStore := &panel{opts: Options{Registry: registryOf(plain)}}
	if caps := noStore.capabilities(context.Background(), plain); caps.ModelPolicyStore {
		t.Error("panel advertised model_policy_store without a store")
	}
}

func TestPanelSavesAndReadsBackSharedModelPolicy(t *testing.T) {
	store := mustPolicyStore(t)
	c := plainPolicyClient("trae", "acct-1")
	h := New(Options{Registry: registryOf(c), Started: time.Now(), ModelPolicies: store})

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/trae/accounts/acct-1/model-policy",
		`{"disabled_models":[" Model-B ","model-a","MODEL-B"],"force_enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := store.Disabled("trae", "acct-1"); strings.Join(got, ",") != "model-a,model-b" {
		t.Fatalf("stored policy = %v, want [model-a model-b]", got)
	}
	var saved struct {
		Accounts []core.AccountRecord `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode save response: %v", err)
	}
	if len(saved.Accounts) != 1 {
		t.Fatalf("save response accounts = %d, want 1", len(saved.Accounts))
	}
	if got := accountDisabled(t, saved.Accounts[0]); strings.Join(got, ",") != "model-a,model-b" {
		t.Errorf("response account disabled_models = %v, want [model-a model-b]", got)
	}
	if _, ok := saved.Accounts[0].Fields["force_enabled"]; ok {
		t.Error("shared-store path wrote force_enabled; only a module can honour that flag")
	}

	// A fresh GET must carry the same blacklist, so a reloaded page repaints the
	// dialog with the model still unchecked.
	rec = hitRoute(t, h, http.MethodGet, "/panel/api/clients/trae/accounts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Accounts     []core.AccountRecord `json:"accounts"`
		Capabilities core.Capabilities    `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if !listed.Capabilities.ModelPolicyStore {
		t.Error("list response did not advertise model_policy_store")
	}
	if got := accountDisabled(t, listed.Accounts[0]); strings.Join(got, ",") != "model-a,model-b" {
		t.Errorf("listed disabled_models = %v, want [model-a model-b]", got)
	}

	// An empty blacklist clears the entry, and a second GET shows the account as
	// fully open rather than keeping a stale field.
	rec = hitRoute(t, h, http.MethodPost, "/panel/api/clients/trae/accounts/acct-1/model-policy", `{"disabled_models":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := store.Disabled("trae", "acct-1"); len(got) != 0 {
		t.Errorf("cleared policy = %v, want empty", got)
	}
	rec = hitRoute(t, h, http.MethodGet, "/panel/api/clients/trae/accounts", "")
	var cleared struct {
		Accounts []core.AccountRecord `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cleared); err != nil {
		t.Fatalf("decode cleared list: %v", err)
	}
	if got := accountDisabled(t, cleared.Accounts[0]); len(got) != 0 {
		t.Errorf("cleared account still reports disabled_models = %v", got)
	}
}

func TestPanelRefusesSharedPolicyForAnUnknownAccount(t *testing.T) {
	store := mustPolicyStore(t)
	c := plainPolicyClient("trae", "acct-1")
	h := New(Options{Registry: registryOf(c), Started: time.Now(), ModelPolicies: store})
	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/trae/accounts/nope/model-policy", `{"disabled_models":["m1"]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body %s", rec.Code, rec.Body.String())
	}
	if got := store.Disabled("trae", "nope"); len(got) != 0 {
		t.Errorf("unknown account wrote policy %v", got)
	}
}

func accountDisabled(t *testing.T, rec core.AccountRecord) []string {
	t.Helper()
	raw, ok := rec.Fields["disabled_models"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		t.Fatalf("disabled_models has type %T", raw)
		return nil
	}
}
