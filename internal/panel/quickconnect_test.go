package panel

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// fakeQuickClient is a module that can offer one-click connections but owns no
// real upstream: enough of AccountManager to satisfy the relist, plus the
// QuickConnectProvider verbs the routes are about.
type fakeQuickClient struct {
	*fakeBareClient
	targets    []core.QuickConnectTarget
	probe      core.QuickConnectStatus
	connect    core.AccountRecord
	connectErr error

	gotProbeID string
	gotFields  map[string]string
}

func (f *fakeQuickClient) AccountFields(context.Context) []core.FieldSpec { return nil }

func (f *fakeQuickClient) Accounts(context.Context) ([]core.AccountRecord, error) {
	if f.connect.ID == "" {
		return []core.AccountRecord{}, nil
	}
	return []core.AccountRecord{f.connect}, nil
}

func (f *fakeQuickClient) AddAccount(context.Context, core.AccountSpec) (core.AccountRecord, error) {
	return core.AccountRecord{}, nil
}

func (f *fakeQuickClient) RemoveAccount(context.Context, string) error { return nil }

func (f *fakeQuickClient) SetAccountEnabled(context.Context, string, bool) error { return nil }

func (f *fakeQuickClient) TestAccount(context.Context, string) (core.TestResult, error) {
	return core.TestResult{}, nil
}

func (f *fakeQuickClient) RefreshAccount(context.Context, string) ([]core.RefreshResult, error) {
	return nil, nil
}

func (f *fakeQuickClient) QuickConnectTargets(context.Context) []core.QuickConnectTarget {
	return f.targets
}

func (f *fakeQuickClient) ProbeQuickConnect(_ context.Context, id string) (core.QuickConnectStatus, error) {
	f.gotProbeID = id
	st := f.probe
	st.ID = id
	return st, nil
}

func (f *fakeQuickClient) ConnectQuickConnect(_ context.Context, id string, fields map[string]string) (core.AccountRecord, error) {
	f.gotFields = fields
	if f.connectErr != nil {
		return core.AccountRecord{}, f.connectErr
	}
	rec := f.connect
	if rec.ID == "" {
		rec.ID = id
	}
	return rec, nil
}

func newQuickClient() *fakeQuickClient {
	return &fakeQuickClient{
		fakeBareClient: &fakeBareClient{name: "openai-compat"},
		targets: []core.QuickConnectTarget{{
			ID:          "omniroute",
			Label:       "OmniRoute",
			BaseURL:     "http://127.0.0.1:20128/v1",
			KeyOptional: true,
		}},
	}
}

func TestQuickConnectProbeRouteReportsRunning(t *testing.T) {
	c := newQuickClient()
	c.probe = core.QuickConnectStatus{Running: true, Detail: "ok"}
	rec, out := doTask(t, taskPanel(t, c), http.MethodPost,
		"/panel/api/clients/openai-compat/quick-connect/omniroute/probe", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("probe = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if out["running"] != true {
		t.Fatalf("body = %v, want running:true", out)
	}
	if c.gotProbeID != "omniroute" {
		t.Fatalf("probe target = %q, want omniroute", c.gotProbeID)
	}
}

func TestQuickConnectConnectRouteCreatesTheSource(t *testing.T) {
	c := newQuickClient()
	c.connect = core.AccountRecord{ID: "omniroute", Label: "OmniRoute", Enabled: true, State: "ready"}
	rec, out := doTask(t, taskPanel(t, c), http.MethodPost,
		"/panel/api/clients/openai-compat/quick-connect/omniroute/connect",
		`{"fields":{"api_key":"sk-1","base_url":"http://127.0.0.1:20128/v1"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("connect = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if c.gotFields["api_key"] != "sk-1" {
		t.Fatalf("connect fields = %v, want the operator's key passed through", c.gotFields)
	}
	acct, _ := out["account"].(map[string]any)
	if acct["id"] != "omniroute" {
		t.Fatalf("account = %v, want the omniroute row", out["account"])
	}
	// The reply must carry the relisted accounts, or the panel cannot paint the
	// new row without a second request shape.
	if _, ok := out["accounts"]; !ok {
		t.Fatalf("body = %v, want an accounts list", out)
	}
}

func TestQuickConnectConnectRefusalIs400(t *testing.T) {
	c := newQuickClient()
	c.connectErr = context.DeadlineExceeded
	rec, _ := doTask(t, taskPanel(t, c), http.MethodPost,
		"/panel/api/clients/openai-compat/quick-connect/omniroute/connect", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("connect = %d (%s), want 400", rec.Code, rec.Body.String())
	}
}

func TestQuickConnectWithoutTheCapabilityIs501(t *testing.T) {
	rec, _ := doTask(t, taskPanel(t, &fakeBareClient{name: "workbuddy"}), http.MethodPost,
		"/panel/api/clients/workbuddy/quick-connect/omniroute/probe", "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("probe = %d (%s), want 501", rec.Code, rec.Body.String())
	}
}

func TestQuickConnectProbeRequiresPost(t *testing.T) {
	rec, _ := doTask(t, taskPanel(t, newQuickClient()), http.MethodGet,
		"/panel/api/clients/openai-compat/quick-connect/omniroute/probe", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET probe = %d (%s), want 405", rec.Code, rec.Body.String())
	}
}

func TestCapabilitiesAdvertiseQuickConnectTargets(t *testing.T) {
	caps := core.CapabilitiesOf(context.Background(), newQuickClient())
	if len(caps.QuickConnect) != 1 {
		t.Fatalf("quick_connect = %v, want one target", caps.QuickConnect)
	}
	if caps.QuickConnect[0].ID != "omniroute" || !caps.QuickConnect[0].KeyOptional {
		t.Fatalf("quick_connect[0] = %+v", caps.QuickConnect[0])
	}
}

// TestAddDialogRendersTheQuickConnectBlock pins the surface: a delegated
// listener (the cards are repainted) and a probe/connect call per target.
// Without it the capability would be advertised but unreachable.
func TestAddDialogRendersTheQuickConnectBlock(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		`id="quickConnect"`,
		`id="qcCards"`,
		`function renderQuickConnect(n) {`,
		`quick_connect`,
		`"/quick-connect/" + aid(card.dataset.qc) + "/probe"`,
		`"/quick-connect/" + aid(card.dataset.qc) + "/connect"`,
		`$("#quickConnect").addEventListener("click"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("quick-connect UI is missing %s", want)
		}
	}
	if manual := poolStatsFuncBody(t, src, "renderManual"); !strings.Contains(manual, "renderQuickConnect(n)") {
		t.Error("renderManual does not paint the quick-connect block")
	}
	pick := poolStatsFuncBody(t, src, "pickAddClient")
	if !strings.Contains(pick, "renderQuickConnect(n)") {
		t.Error("switching client does not repaint the quick-connect block")
	}
}
