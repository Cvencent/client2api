package zcode

// Offline tests for the upstream model catalogue.  Every test here runs against
// net/http/httptest or the package's fakeTransport -- nothing touches the real
// vendor, and nothing touches the developer's own ~/.zcode (isolateHome keeps
// credential discovery pinned to an empty temp dir).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

const modelTestKey = "0123456789abcdef0123456789abcdef"

// anthropicList is the shape api.z.ai/open.bigmodel.cn serve.
const anthropicList = `{"data":[{"id":"glm-4.6","display_name":"GLM-4.6",` +
	`"context_length":200000,"max_output_tokens":131072},{"id":"glm-4.6-flash"}]}`

// authEnvelope is the exact refusal the vendor sends with HTTP 200 when no
// credential was presented.  It must be read as a failure, never as an empty
// catalogue.
const authEnvelope = `{"code":1001,"msg":"Authentication parameter not received in Header, unable to authenticate","success":false}`

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// modelProbe records what an offline vendor actually received.
type modelProbe struct {
	mu      sync.Mutex
	methods []string
	paths   []string
	headers []http.Header
}

func (p *modelProbe) record(r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.methods = append(p.methods, r.Method)
	p.paths = append(p.paths, r.URL.Path)
	p.headers = append(p.headers, r.Header.Clone())
}

func (p *modelProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.paths)
}

func (p *modelProbe) methodAt(i int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < 0 || i >= len(p.methods) {
		return ""
	}
	return p.methods[i]
}

func (p *modelProbe) pathAt(i int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < 0 || i >= len(p.paths) {
		return ""
	}
	return p.paths[i]
}

func (p *modelProbe) headerAt(i int) http.Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < 0 || i >= len(p.headers) {
		return http.Header{}
	}
	return p.headers[i]
}

// modelServer starts an offline vendor and records every request it receives.
func modelServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *modelProbe) {
	t.Helper()
	probe := &modelProbe{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probe.record(r)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, probe
}

// writeJSON answers with a JSON body.  It never calls t.Fatalf: it runs on the
// server's own goroutine.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// newModelTestClient builds a client wired to a REAL http.Client, so the
// offline httptest server is reachable.  Credential discovery is isolated to an
// empty home first.
func newModelTestClient(t *testing.T, configJSON string) *Client {
	t.Helper()
	isolateHome(t)
	c, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(configJSON),
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client, ok := c.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", c)
	}
	return client
}

// apiKeyConfig points one api_key account at base.
func apiKeyConfig(t *testing.T, base string) string {
	t.Helper()
	return fmt.Sprintf(`{"auto_discover":false,"accounts":[`+
		`{"id":"k1","label":"z.ai key","provider":"zai","mode":"api_key",`+
		`"api_key":%q,"base_url":%q}]}`, modelTestKey, base)
}

// jwtConfig points one jwt account at base.  A captcha command is present so
// the account is selectable at all; the model list never runs the solver.
func jwtConfig(t *testing.T, base, jwt string) string {
	t.Helper()
	return fmt.Sprintf(`{"auto_discover":false,"captcha_command":"noop-solver","accounts":[`+
		`{"id":"j1","label":"z.ai jwt","provider":"zai","mode":"jwt",`+
		`"jwt":%q,"base_url":%q}]}`, jwt, base)
}

// configuredIDs is the fallback catalogue: the built-in default list.
func configuredIDs() []string {
	return append([]string(nil), defaultModels...)
}

func assertConfigured(t *testing.T, models []core.Model) {
	t.Helper()
	ids := modelIDsOf(models)
	want := configuredIDs()
	if len(ids) != len(want) {
		t.Fatalf("models = %v, want the configured list %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("models = %v, want the configured list %v", ids, want)
		}
	}
}

func modelIDsOf(models []core.Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

func assertIDs(t *testing.T, models []core.Model, want ...string) {
	t.Helper()
	got := modelIDsOf(models)
	if len(got) != len(want) {
		t.Fatalf("model ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("model ids = %v, want %v", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

func TestRefreshModelsFetchesTheVendorsCatalogue(t *testing.T) {
	srv, probe := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("upstream method = %s, want GET", r.Method)
		}
		writeJSON(w, http.StatusOK, anthropicList)
	})
	c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))

	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	assertIDs(t, models, "glm-4.6", "glm-4.6-flash")

	for _, m := range models {
		if m.OwnedBy != "zcode" {
			t.Errorf("OwnedBy = %q, want %q", m.OwnedBy, "zcode")
		}
	}
	if got := models[0].Extra["display_name"]; got != "GLM-4.6" {
		t.Errorf("display_name = %v, want GLM-4.6", got)
	}
	if got := models[0].Extra["context_length"]; got != int64(200000) {
		t.Errorf("context_length = %v (%T), want int64(200000)", got, got)
	}
	if got := models[0].Extra["max_output_tokens"]; got != int64(131072) {
		t.Errorf("max_output_tokens = %v (%T), want int64(131072)", got, got)
	}
	// The vendor sent no display name or limits for the second model, so
	// nothing may be invented for it.
	if models[1].Extra != nil {
		t.Errorf("Extra = %v, want nil when the vendor sent nothing", models[1].Extra)
	}

	if probe.count() != 1 {
		t.Fatalf("upstream requests = %d, want 1", probe.count())
	}
	if got := probe.methodAt(0); got != http.MethodGet {
		t.Errorf("recorded method = %s, want GET", got)
	}
	if got := probe.pathAt(0); got != "/api/anthropic/v1/models" {
		t.Errorf("recorded path = %s, want /api/anthropic/v1/models", got)
	}
	h := probe.headerAt(0)
	if got := h.Get("X-Api-Key"); got != modelTestKey {
		t.Errorf("x-api-key = %q, want the account's key", got)
	}
	if got := h.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}
	if got := h.Get("User-Agent"); got != defaultUserAgent {
		t.Errorf("user-agent = %q, want %q", got, defaultUserAgent)
	}
	if got := h.Get("X-ZCode-Agent"); got != defaultAgent {
		t.Errorf("x-zcode-agent = %q, want %q", got, defaultAgent)
	}
}

func TestModelsServesUpstreamFromTheCache(t *testing.T) {
	srv, probe := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, anthropicList)
	})
	c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))
	ctx := context.Background()

	first, err := c.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	assertIDs(t, first, "glm-4.6", "glm-4.6-flash")
	if probe.count() != 1 {
		t.Fatalf("upstream requests after the first Models() = %d, want 1", probe.count())
	}

	// The panel calls Models() on every refresh; it must be answered from the
	// cache, not from the vendor.
	for i := 0; i < 5; i++ {
		again, err := c.Models(ctx)
		if err != nil {
			t.Fatalf("Models (cached): %v", err)
		}
		assertIDs(t, again, "glm-4.6", "glm-4.6-flash")
	}
	if probe.count() != 1 {
		t.Fatalf("upstream requests = %d, want 1 (the TTL must suppress re-fetches)", probe.count())
	}
}

func TestTTLSuppressesTheSecondFetch(t *testing.T) {
	srv, probe := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, anthropicList)
	})
	c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))
	ctx := context.Background()

	if _, err := c.Models(ctx); err != nil {
		t.Fatalf("Models: %v", err)
	}
	now := time.Now()
	c.models.mu.Lock()
	stored := c.models.storedAt
	c.models.mu.Unlock()
	if stored.IsZero() {
		t.Fatal("the successful fetch did not populate the cache")
	}
	if age := now.Sub(stored); age >= modelsCacheTTL {
		t.Fatalf("cache age = %v, want it inside the %v TTL", age, modelsCacheTTL)
	}
	if !c.models.triedRecently(now, modelsCacheTTL) {
		t.Fatal("a fresh attempt must count as recently tried")
	}

	if _, err := c.Models(ctx); err != nil {
		t.Fatalf("Models (cached): %v", err)
	}
	if probe.count() != 1 {
		t.Fatalf("upstream requests = %d, want 1", probe.count())
	}

	// RefreshModels is the explicit "re-fetch now" action: it bypasses the TTL.
	if _, err := c.RefreshModels(ctx); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if probe.count() != 2 {
		t.Fatalf("upstream requests = %d, want 2 after an explicit refresh", probe.count())
	}
}

// ---------------------------------------------------------------------------
// The HTTP-200 auth envelope
// ---------------------------------------------------------------------------

func TestHTTP200AuthEnvelopeIsAFailure(t *testing.T) {
	srv, probe := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, authEnvelope)
	})
	c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))
	ctx := context.Background()

	// Models() stays usable: the configured list survives.
	models, err := c.Models(ctx)
	if err != nil {
		t.Fatalf("Models must not surface the upstream failure: %v", err)
	}
	assertConfigured(t, models)

	// RefreshModels reports it, and still hands back a usable catalogue.
	refreshed, err := c.RefreshModels(ctx)
	if err != nil {
		// expected
	} else {
		t.Fatal("RefreshModels accepted a 200 auth envelope as success")
	}
	if !strings.Contains(err.Error(), "1001") {
		t.Errorf("error = %q, want it to name the vendor code 1001", err.Error())
	}
	if !strings.Contains(err.Error(), "unable to authenticate") {
		t.Errorf("error = %q, want it to carry the vendor's message", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "zcode: ") {
		t.Errorf("error = %q, want the zcode: prefix", err.Error())
	}
	assertConfigured(t, refreshed)
	if len(refreshed) == 0 {
		t.Fatal("a failed refresh emptied the catalogue")
	}

	if probe.count() < 1 {
		t.Fatal("no upstream request was attempted")
	}
}

// ---------------------------------------------------------------------------
// Fallbacks
// ---------------------------------------------------------------------------

func TestModelListHTTPFailuresFallBackToConfiguredList(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"code":"1001","message":"Authentication parameter not received in Header, unable to authenticate"}}`},
		{"forbidden", http.StatusForbidden, `{"error":{"code":"1002","message":"no permission"}}`},
		{"server_error", http.StatusInternalServerError, `{"error":{"message":"boom"}}`},
		{"bad_gateway", http.StatusBadGateway, `<html>502</html>`},
		{"not_found", http.StatusNotFound, `{"code":1001,"msg":"not found","success":false}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, probe := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, tc.code, tc.body)
			})
			c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))

			models, err := c.Models(context.Background())
			if err != nil {
				t.Fatalf("Models: %v", err)
			}
			assertConfigured(t, models)
			if probe.count() != 1 {
				t.Fatalf("upstream requests = %d, want 1", probe.count())
			}

			// The refusal must be reported by the explicit refresh, with the
			// status in it.
			_, err = c.RefreshModels(context.Background())
			if err == nil {
				t.Fatal("RefreshModels reported success for a failing vendor")
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", tc.code)) {
				t.Errorf("error = %q, want it to carry HTTP %d", err.Error(), tc.code)
			}
		})
	}
}

func TestModelsWithNoAccountNeverTouchesTheNetwork(t *testing.T) {
	// A fakeTransport with no handler fails loudly if anything dials it, so a
	// request count of zero is a real assertion.
	transport := &fakeTransport{}
	c := newTestClient(t, `{"auto_discover":false}`, transport)
	ctx := context.Background()

	models, err := c.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	assertConfigured(t, models)
	if transport.count() != 0 {
		t.Fatalf("Models() made %d network attempts with no account, want 0", transport.count())
	}

	refreshed, err := c.RefreshModels(ctx)
	if err == nil {
		t.Fatal("RefreshModels succeeded with no account to use")
	}
	if !strings.HasPrefix(err.Error(), "zcode: ") {
		t.Errorf("error = %q, want the zcode: prefix", err.Error())
	}
	assertConfigured(t, refreshed)
	if transport.count() != 0 {
		t.Fatalf("RefreshModels made %d network attempts with no account, want 0", transport.count())
	}
}

func TestDisabledAccountIsNotUsed(t *testing.T) {
	srv, probe := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, anthropicList)
	})
	cfg := fmt.Sprintf(`{"auto_discover":false,"accounts":[`+
		`{"id":"k1","provider":"zai","mode":"api_key","api_key":%q,"base_url":%q,"enabled":false}]}`,
		modelTestKey, srv.URL+"/api/anthropic")
	c := newModelTestClient(t, cfg)

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	assertConfigured(t, models)
	if probe.count() != 0 {
		t.Fatalf("upstream requests = %d, want 0 for a disabled account", probe.count())
	}
}

// ---------------------------------------------------------------------------
// The catalogue is never emptied
// ---------------------------------------------------------------------------

func TestRefreshModelsFailureKeepsTheLastGoodList(t *testing.T) {
	var failing atomic.Bool
	srv, _ := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			writeJSON(w, http.StatusInternalServerError, `{"error":{"message":"upstream on fire"}}`)
			return
		}
		writeJSON(w, http.StatusOK, anthropicList)
	})
	c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))
	ctx := context.Background()

	good, err := c.RefreshModels(ctx)
	if err != nil {
		t.Fatalf("first RefreshModels: %v", err)
	}
	assertIDs(t, good, "glm-4.6", "glm-4.6-flash")

	failing.Store(true)

	after, err := c.RefreshModels(ctx)
	if err == nil {
		t.Fatal("RefreshModels reported success while upstream was failing")
	}
	// The last good list -- NOT the configured fallback -- must come back with
	// the error, so a flaky refresh cannot blank the model picker.
	assertIDs(t, after, "glm-4.6", "glm-4.6-flash")

	// Models() keeps serving the good list too.
	served, err := c.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	assertIDs(t, served, "glm-4.6", "glm-4.6-flash")
}

func TestMalformedModelListDoesNotEmptyTheCatalogue(t *testing.T) {
	bodies := []string{
		`{"data":`,
		`not json at all`,
		`<html><body>maintenance</body></html>`,
		`{"data":[{"id":123}]}`,
		`{}`,
		`{"data":[]}`,
		`{"data":[{"id":""},{"id":"   "}]}`,
		`null`,
		``,
	}
	for i, body := range bodies {
		t.Run(fmt.Sprintf("body_%d", i), func(t *testing.T) {
			srv, _ := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, body)
			})
			c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))
			ctx := context.Background()

			models, err := c.Models(ctx)
			if err != nil {
				t.Fatalf("Models: %v", err)
			}
			assertConfigured(t, models)

			refreshed, err := c.RefreshModels(ctx)
			if err == nil {
				t.Fatalf("RefreshModels accepted %q as a model list", body)
			}
			assertConfigured(t, refreshed)
		})
	}
}

// ---------------------------------------------------------------------------
// Shapes
// ---------------------------------------------------------------------------

func TestParseModelListShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"anthropic", `{"data":[{"id":"a"},{"id":"b"}]}`, []string{"a", "b"}},
		{"zai_envelope", `{"code":0,"msg":"","success":true,"data":[{"id":"a"}]}`, []string{"a"}},
		{"zai_no_msg", `{"code":0,"data":[{"id":"a"}]}`, []string{"a"}},
		{"openai_models", `{"models":[{"id":"a"},{"id":"b"}]}`, []string{"a", "b"}},
		{"wrapped_models", `{"data":{"models":[{"id":"a"}]}}`, []string{"a"}},
		{"wrapped_data", `{"data":{"data":[{"id":"a"}]}}`, []string{"a"}},
		{"openai_code_200", `{"code":200,"success":true,"data":[{"id":"a"}]}`, []string{"a"}},
		{"deduplicated", `{"data":[{"id":"a"},{"id":"a"},{"id":"b"}]}`, []string{"a", "b"}},
		{"name_fallback", `{"data":[{"id":"a","name":"A"}]}`, []string{"a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models, err := parseModelList([]byte(tc.body), "zcode")
			if err != nil {
				t.Fatalf("parseModelList(%s): %v", tc.body, err)
			}
			assertIDs(t, models, tc.want...)
		})
	}
}

func TestParseModelListRejections(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{"auth_envelope", authEnvelope, "1001"},
		{"auth_envelope_with_entries", `{"code":1001,"msg":"unable to authenticate","success":false,"data":[{"id":"a"}]}`, "1001"},
		{"string_code", `{"code":"1001","msg":"nope","data":[{"id":"a"}]}`, "1001"},
		{"success_false", `{"success":false,"msg":"credential expired","data":[{"id":"a"}]}`, "credential expired"},
		{"error_object", `{"error":{"code":"1001","message":"nope"},"data":[{"id":"a"}]}`, "nope"},
		{"empty_data", `{"data":[]}`, "no models"},
		{"empty_object", `{}`, "no models"},
		{"malformed", `{"data":`, "not JSON"},
		{"blank_ids", `{"data":[{"id":"  "}]}`, "no models"},
		{"not_an_object", `[1,2,3]`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models, err := parseModelList([]byte(tc.body), "zcode")
			if err == nil {
				t.Fatalf("parseModelList(%s) = %v, want an error", tc.body, modelIDsOf(models))
			}
			if !strings.HasPrefix(err.Error(), "zcode: ") {
				t.Errorf("error = %q, want the zcode: prefix", err.Error())
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestParseModelListNeverInventsValues(t *testing.T) {
	models, err := parseModelList([]byte(`{"data":[{"id":"a"},{"id":"b","display_name":"B","context_length":"128000","max_tokens":4096}]}`), "zcode")
	if err != nil {
		t.Fatalf("parseModelList: %v", err)
	}
	assertIDs(t, models, "a", "b")
	if models[0].Extra != nil {
		t.Errorf("Extra for a bare id = %v, want nil", models[0].Extra)
	}
	if got := models[1].Extra["display_name"]; got != "B" {
		t.Errorf("display_name = %v, want B", got)
	}
	// A numeric field sent as a string is coerced, not dropped.
	if got := models[1].Extra["context_length"]; got != int64(128000) {
		t.Errorf("context_length = %v (%T), want int64(128000)", got, got)
	}
	if got := models[1].Extra["max_output_tokens"]; got != int64(4096) {
		t.Errorf("max_output_tokens = %v (%T), want int64(4096)", got, got)
	}
	if _, ok := models[1].Extra["max_tokens"]; ok {
		t.Error("Extra kept the vendor's raw max_tokens key instead of max_output_tokens")
	}
	// A display name identical to the id carries no information.
	same, err := parseModelList([]byte(`{"data":[{"id":"a","display_name":"a"}]}`), "zcode")
	if err != nil {
		t.Fatalf("parseModelList: %v", err)
	}
	if same[0].Extra != nil {
		t.Errorf("Extra = %v, want nil when display_name duplicates the id", same[0].Extra)
	}
}

func TestJoinModels(t *testing.T) {
	cases := map[string]string{
		"https://api.z.ai/api/anthropic":           "https://api.z.ai/api/anthropic/v1/models",
		"https://api.z.ai/api/anthropic/":          "https://api.z.ai/api/anthropic/v1/models",
		"https://api.z.ai/api/anthropic/v1/models": "https://api.z.ai/api/anthropic/v1/models",
		"https://api.z.ai/api/anthropic/v1":        "https://api.z.ai/api/anthropic/v1/models",
		"":                                         "",
		"   ":                                      "",
	}
	for in, want := range cases {
		if got := joinModels(in); got != want {
			t.Errorf("joinModels(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Signing
// ---------------------------------------------------------------------------

func TestRefreshModelsSignsLikeTheAccountMode(t *testing.T) {
	t.Run("api_key", func(t *testing.T) {
		srv, probe := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, anthropicList)
		})
		c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))

		if _, err := c.RefreshModels(context.Background()); err != nil {
			t.Fatalf("RefreshModels: %v", err)
		}
		h := probe.headerAt(0)
		if got := h.Get("X-Api-Key"); got != modelTestKey {
			t.Errorf("x-api-key = %q, want the account's key", got)
		}
		if got := h.Get("Authorization"); got != "" {
			t.Errorf("authorization = %q, want it unset in api_key mode", got)
		}
	})

	t.Run("jwt", func(t *testing.T) {
		srv, probe := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, anthropicList)
		})
		jwt := makeJWT(`{"exp":9999999999,"userId":"u1"}`)
		c := newModelTestClient(t, jwtConfig(t, srv.URL+"/api/anthropic", jwt))

		if _, err := c.RefreshModels(context.Background()); err != nil {
			t.Fatalf("RefreshModels: %v", err)
		}
		if probe.count() != 1 {
			t.Fatalf("upstream requests = %d, want 1", probe.count())
		}
		h := probe.headerAt(0)
		if got := h.Get("Authorization"); got != "Bearer "+jwt {
			t.Errorf("authorization = %q, want a Bearer token", got)
		}
		if got := h.Get("X-Api-Key"); got != "" {
			t.Errorf("x-api-key = %q, want it unset in jwt mode", got)
		}
		if got := h.Get("X-Zcode-Session-Type"); got != "main" {
			t.Errorf("x-zcode-session-type = %q, want main", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Secrets
// ---------------------------------------------------------------------------

func TestModelErrorsNeverLeakTheCredential(t *testing.T) {
	leaky := func(secret string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// A hostile/buggy vendor quoting the credential back at us.
			writeJSON(w, http.StatusOK, fmt.Sprintf(
				`{"code":1001,"msg":"key %s is not authorised","success":false}`, secret))
		}
	}

	t.Run("api_key", func(t *testing.T) {
		srv, _ := modelServer(t, leaky(modelTestKey))
		c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))
		_, err := c.RefreshModels(context.Background())
		if err == nil {
			t.Fatal("RefreshModels accepted the refusal")
		}
		if strings.Contains(err.Error(), modelTestKey) {
			t.Fatalf("error leaked the API key: %q", err.Error())
		}
	})

	t.Run("api_key_401", func(t *testing.T) {
		srv, _ := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusUnauthorized, fmt.Sprintf(
				`{"error":{"code":"1001","message":"key %s rejected"}}`, modelTestKey))
		})
		c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))
		_, err := c.RefreshModels(context.Background())
		if err == nil {
			t.Fatal("RefreshModels accepted the 401")
		}
		if strings.Contains(err.Error(), modelTestKey) {
			t.Fatalf("error leaked the API key: %q", err.Error())
		}
	})

	t.Run("jwt", func(t *testing.T) {
		jwt := makeJWT(`{"exp":9999999999,"userId":"u1"}`)
		srv, _ := modelServer(t, leaky(jwt))
		c := newModelTestClient(t, jwtConfig(t, srv.URL+"/api/anthropic", jwt))
		_, err := c.RefreshModels(context.Background())
		if err == nil {
			t.Fatal("RefreshModels accepted the refusal")
		}
		if strings.Contains(err.Error(), jwt) {
			t.Fatalf("error leaked the JWT: %q", err.Error())
		}
	})
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

func TestModelCacheIsSafeForConcurrentUse(t *testing.T) {
	srv, _ := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, anthropicList)
	})
	c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				models, err := c.Models(context.Background())
				if err != nil {
					t.Errorf("Models: %v", err)
					return
				}
				if len(models) == 0 {
					t.Error("a concurrent Models() call saw an empty catalogue")
					return
				}
				if _, err := c.RefreshModels(context.Background()); err != nil {
					t.Errorf("RefreshModels: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	final, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	assertIDs(t, final, "glm-4.6", "glm-4.6-flash")
}

func TestConcurrentRefreshIsSerialised(t *testing.T) {
	var inFlight, maxInFlight atomic.Int32
	srv, probe := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			cur := maxInFlight.Load()
			if n <= cur || maxInFlight.CompareAndSwap(cur, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		inFlight.Add(-1)
		writeJSON(w, http.StatusOK, anthropicList)
	})
	c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.RefreshModels(context.Background()); err != nil {
				t.Errorf("RefreshModels: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := maxInFlight.Load(); got > 1 {
		t.Errorf("concurrent upstream requests = %d, want at most 1", got)
	}
	if probe.count() == 0 {
		t.Fatal("no upstream request was made at all")
	}
}

// ---------------------------------------------------------------------------
// Helpers and edge cases
// ---------------------------------------------------------------------------

func TestModelsFallsBackToConfiguredListWhenUpstreamHangs(t *testing.T) {
	release := make(chan struct{})
	srv, _ := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		writeJSON(w, http.StatusOK, anthropicList)
	})
	t.Cleanup(func() { close(release) })
	c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	models, err := c.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	assertConfigured(t, models)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Models blocked for %v on a hanging vendor", elapsed)
	}
}

func TestConfiguredModelsSkipsBlankIDs(t *testing.T) {
	c := newModelTestClient(t, `{"auto_discover":false,"models":["GLM-5.3","","   ","GLM-4.6"]}`)
	models := c.configuredModels()
	assertIDs(t, models, "GLM-5.3", "GLM-4.6")
	for _, m := range models {
		if m.OwnedBy != "zcode" {
			t.Errorf("OwnedBy = %q, want zcode", m.OwnedBy)
		}
	}
}

func TestConfiguredModelsIsTheDefaultList(t *testing.T) {
	c := newModelTestClient(t, `{"auto_discover":false}`)
	assertConfigured(t, c.configuredModels())
}

func TestModelCacheCloneIsIndependent(t *testing.T) {
	mc := newModelCache()
	now := time.Now()
	ref, claimed := mc.beginRefresh()
	if !claimed {
		t.Fatal("the first refresh was not claimed")
	}
	mc.finishRefresh(ref, []core.Model{{ID: "a", OwnedBy: "zcode"}}, nil, now)

	first := mc.snapshot()
	first[0].ID = "mutated"
	second := mc.snapshot()
	if second[0].ID != "a" {
		t.Fatalf("snapshot aliased the cache: got %q, want a", second[0].ID)
	}

	// A refresh that brings back nothing must leave the last good list in
	// place: this is the invariant that keeps a flaky vendor from blanking the
	// model picker.
	ref, claimed = mc.beginRefresh()
	if !claimed {
		t.Fatal("the second refresh was not claimed")
	}
	mc.finishRefresh(ref, nil, errors.New("zcode: upstream on fire"), now.Add(time.Second))
	kept := mc.snapshot()
	if len(kept) != 1 || kept[0].ID != "a" {
		t.Fatalf("snapshot = %v, want the last good list [a]", modelIDsOf(kept))
	}
	if _, ok := mc.cached(now.Add(time.Second), modelsCacheTTL); !ok {
		t.Error("a failed refresh discarded the cached catalogue")
	}

	// A second refresh while one is in flight joins it instead of dialling.
	ref, claimed = mc.beginRefresh()
	if !claimed {
		t.Fatal("the third refresh was not claimed")
	}
	joined, ok := mc.beginRefresh()
	if ok {
		t.Fatal("a concurrent refresh was allowed to start a second request")
	}
	if joined != ref {
		t.Fatal("the joiner did not receive the in-flight refresh")
	}
	mc.finishRefresh(ref, nil, errors.New("zcode: upstream on fire"), now.Add(2*time.Second))
	select {
	case <-joined.done:
	default:
		t.Fatal("finishing the refresh did not release its joiners")
	}
}

func TestModelsRefresherInterfaceIsSatisfied(t *testing.T) {
	var _ core.ModelRefresher = (*Client)(nil)
	var _ core.Client = (*Client)(nil)
	c := newModelTestClient(t, `{"auto_discover":false}`)
	if !core.CapabilitiesOf(context.Background(), c).Refresh {
		t.Error("Capabilities.Refresh = false for a module implementing ModelRefresher")
	}
}

// TestBuiltinSpecsFillTheVendorsSilentBudget pins the fix for the silent
// truncation: the plan's /v1/models list names each model and nothing else, so
// a catalogue entry has to pick the published budget up from
// builtinModelSpecs, or every caller that omitted max_tokens is cut off at
// the module's flat default.
func TestBuiltinSpecsFillTheVendorsSilentBudget(t *testing.T) {
	models, err := parseModelList([]byte(`{"data":[{"id":"glm-5.3-flash"},{"id":"GLM-4.6"}]}`), "zcode")
	if err != nil {
		t.Fatalf("parseModelList: %v", err)
	}
	if got, ok := core.ModelOutputLimit(models[0]); !ok || got != 131072 {
		t.Errorf("glm-5.3-flash max output = %d (ok=%v), want 131072", got, ok)
	}
	if got := models[0].Extra["context_length"]; got != 1048576 {
		t.Errorf("glm-5.3-flash context = %v, want 1048576", got)
	}
	// The lookup is case-insensitive: a vendor that capitalises an id still
	// gets the published budget.
	if got, ok := core.ModelOutputLimit(models[1]); !ok || got != 131072 {
		t.Errorf("GLM-4.6 max output = %d (ok=%v), want 131072", got, ok)
	}
	// A number the vendor published always beats the table.
	override, err := parseModelList([]byte(`{"data":[{"id":"glm-5.3-flash","max_output_tokens":8192}]}`), "zcode")
	if err != nil {
		t.Fatalf("parseModelList: %v", err)
	}
	if got, _ := core.ModelOutputLimit(override[0]); got != 8192 {
		t.Errorf("the vendor's own budget was overwritten: %d, want 8192", got)
	}
	// An id the table does not know keeps its old behaviour: nothing invented.
	unknown, err := parseModelList([]byte(`{"data":[{"id":"glm-9.9"}]}`), "zcode")
	if err != nil {
		t.Fatalf("parseModelList: %v", err)
	}
	if unknown[0].Extra != nil {
		t.Errorf("unknown id Extra = %v, want nil", unknown[0].Extra)
	}
}

// TestConfiguredModelsCarryTheBuiltinBudget covers the floor the catalogue
// falls back to when upstream has never answered: an operator-typed id that
// matches a plan model still reaches the gateway with a usable budget.
func TestConfiguredModelsCarryTheBuiltinBudget(t *testing.T) {
	c := newModelTestClient(t, `{"auto_discover":false,"models":["GLM-5.3-Flash"]}`)
	models := c.configuredModels()
	if len(models) != 1 {
		t.Fatalf("configuredModels = %v, want one entry", modelIDsOf(models))
	}
	if got, ok := core.ModelOutputLimit(models[0]); !ok || got != 131072 {
		t.Errorf("configured max output = %d (ok=%v), want 131072", got, ok)
	}
}

// TestEveryDefaultModelHasBuiltinSpec keeps the advertised fallback catalogue
// and the published metadata table from drifting apart.  A model without a
// spec would be shown to callers but would silently fall back to
// defaultMaxTokens when max_tokens is omitted.
func TestEveryDefaultModelHasBuiltinSpec(t *testing.T) {
	for _, id := range defaultModels {
		if _, ok := builtinModelSpecs[strings.ToLower(id)]; !ok {
			t.Errorf("default model %q has no builtinModelSpec", id)
		}
	}
}
