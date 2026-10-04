package kimi

// Tests for the upstream model list (models.go).
//
// Everything here is offline: the vendor is a net/http/httptest server, and no
// test ever reaches api.kimi.com.  The credentials the developer may happen to
// have on this machine are isolated away by isolateCredentials, so a real login
// can neither satisfy nor break a case.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Stub upstream
// ---------------------------------------------------------------------------

// modelStub is a stand-in for the vendor's coding API.  It records what it was
// asked so a test can assert that no request happened at all.
type modelStub struct {
	srv *httptest.Server

	mu      sync.Mutex
	respond func() (int, string)
	hits    int
	auths   []string
	accepts []string
	paths   []string
}

func newModelStub(t *testing.T, respond func() (int, string)) *modelStub {
	t.Helper()
	s := &modelStub{respond: respond}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.accepts = append(s.accepts, r.Header.Get("Accept"))
		s.paths = append(s.paths, r.URL.Path)
		respond := s.respond
		s.mu.Unlock()

		code, body := respond()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *modelStub) setRespond(fn func() (int, string)) {
	s.mu.Lock()
	s.respond = fn
	s.mu.Unlock()
}

func (s *modelStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func (s *modelStub) lastAuth() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.auths) == 0 {
		return ""
	}
	return s.auths[len(s.auths)-1]
}

func (s *modelStub) lastAccept() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.accepts) == 0 {
		return ""
	}
	return s.accepts[len(s.accepts)-1]
}

func (s *modelStub) lastPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.paths) == 0 {
		return ""
	}
	return s.paths[len(s.paths)-1]
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// modelsClient builds a Client pointed at apiBase, optionally with a token in
// the module's own store, and with every real credential location hidden.
func modelsClient(t *testing.T, cfg map[string]any, token string) *Client {
	t.Helper()
	isolateCredentials(t)
	c, _ := newClient(t, cfg)
	if token != "" {
		if err := c.saveToken(storedToken{AccessToken: token, OAuthHost: "https://auth.kimi.com"}); err != nil {
			t.Fatalf("saveToken: %v", err)
		}
	}
	return c
}

func idsOf(models []core.Model) string { return strings.Join(modelIDs(models), ",") }

// builtinIDs is the fallback catalogue as a string, so a test can say "the
// baseline came back" without re-deriving it.
func builtinIDs() string { return idsOf(builtinCatalog()) }

// ---------------------------------------------------------------------------
// The happy path
// ---------------------------------------------------------------------------

func TestModelsReportsUpstreamList(t *testing.T) {
	stub := newModelStub(t, func() (int, string) {
		return http.StatusOK, `{"object":"list","data":[` +
			`{"id":"kimi-k2.5","object":"model","owned_by":"moonshot"},` +
			`{"id":"kimi-k3","owned_by":"moonshot","context_length":262144,"max_output_tokens":32768,"display_name":"Kimi K3"}` +
			`]}`
	})
	// api_base carries the version segment, exactly as the vendor's own root
	// does; apiBase() must be reused rather than a second region rule invented.
	const tok = "panel-access-token-7c1d"
	c := modelsClient(t, map[string]any{"api_base": stub.srv.URL + "/coding/v1"}, tok)

	got, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if ids := idsOf(got); ids != "kimi-k2.5,kimi-k3" {
		t.Fatalf("models = %q, want the upstream ids", ids)
	}
	for _, m := range got {
		if m.OwnedBy != ownerKimi {
			t.Errorf("model %q owned_by = %q, want %q (the builtinCatalog value)", m.ID, m.OwnedBy, ownerKimi)
		}
		if m.Extra["source"] != modelsSourceUpstream {
			t.Errorf("model %q source = %v, want %q", m.ID, m.Extra["source"], modelsSourceUpstream)
		}
	}
	if v := got[1].Extra["context_length"]; v != 262144 {
		t.Errorf("context_length = %v (%T), want 262144", v, v)
	}
	if v := got[1].Extra["max_output_tokens"]; v != 32768 {
		t.Errorf("max_output_tokens = %v (%T), want 32768", v, v)
	}
	if v := got[1].Extra["display_name"]; v != "Kimi K3" {
		t.Errorf("display_name = %v, want %q", v, "Kimi K3")
	}
	// Nothing was invented for the entry that carried no capability fields.
	if v, ok := got[0].Extra["context_length"]; ok {
		t.Errorf("a field the vendor did not send appeared anyway: context_length=%v", v)
	}

	if n := stub.count(); n != 1 {
		t.Fatalf("upstream saw %d requests, want 1", n)
	}
	if a := stub.lastAuth(); a != "Bearer "+tok {
		t.Errorf("Authorization = %q, want the bearer token", a)
	}
	if a := stub.lastAccept(); a != "application/json" {
		t.Errorf("Accept = %q, want application/json", a)
	}
	if p := stub.lastPath(); p != "/coding/v1/models" {
		t.Errorf("path = %q, want /coding/v1/models", p)
	}

	// The capability the panel keys the re-fetch button off must be advertised.
	if caps := core.CapabilitiesOf(context.Background(), c); !caps.Refresh {
		t.Error("CapabilitiesOf().Refresh = false, want true for a ModelRefresher")
	}
}

func TestModelsToleratesSiblingKeyAndBareIDs(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "models key", body: `{"models":[{"id":"alpha"},{"id":"beta"}]}`, want: "alpha,beta"},
		{name: "bare ids", body: `{"data":["alpha","beta"]}`, want: "alpha,beta"},
		{name: "bare array", body: `[{"id":"alpha"}]`, want: "alpha"},
		{name: "data wins", body: `{"data":[{"id":"alpha"}],"models":[{"id":"beta"}]}`, want: "alpha"},
		{name: "empty data falls through", body: `{"data":[],"models":[{"id":"beta"}]}`, want: "beta"},
		{name: "junk entries dropped", body: `{"data":[{"id":"alpha"},{"nope":1},"","beta",null]}`, want: "alpha,beta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newModelStub(t, func() (int, string) { return http.StatusOK, tc.body })
			c := modelsClient(t, map[string]any{"api_base": stub.srv.URL}, "tok-shapes-1234567")
			got, err := c.Models(context.Background())
			if err != nil {
				t.Fatalf("Models: %v", err)
			}
			if ids := idsOf(got); ids != tc.want {
				t.Fatalf("models = %q, want %q", ids, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Fallbacks
// ---------------------------------------------------------------------------

func TestModelsWithoutCredentialNeverCallsUpstream(t *testing.T) {
	stub := newModelStub(t, func() (int, string) {
		return http.StatusOK, `{"data":[{"id":"upstream-only"}]}`
	})
	c := modelsClient(t, map[string]any{"api_base": stub.srv.URL}, "")

	got, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if ids := idsOf(got); ids != builtinIDs() {
		t.Fatalf("models = %q, want the built-in catalogue %q", ids, builtinIDs())
	}
	if n := stub.count(); n != 0 {
		t.Fatalf("upstream saw %d requests with no credential, want 0", n)
	}

	// The explicit refresh button has nothing to authenticate with either, and
	// that is not an error: it answers offline.
	refreshed, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels with no credential: %v", err)
	}
	if ids := idsOf(refreshed); ids != builtinIDs() {
		t.Fatalf("RefreshModels = %q, want the built-in catalogue", ids)
	}
	if n := stub.count(); n != 0 {
		t.Fatalf("upstream saw %d requests after RefreshModels, want 0", n)
	}
}

func TestModelsUpstream401KeepsBaseline(t *testing.T) {
	stub := newModelStub(t, func() (int, string) {
		// Exactly what an unauthenticated probe of the real endpoint returns.
		return http.StatusUnauthorized, `{"error":{"message":"Invalid Authentication","type":"invalid_authentication_error"}}`
	})
	c := modelsClient(t, map[string]any{"api_base": stub.srv.URL}, "stale-panel-token-42")

	got, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models must not fail when upstream refuses: %v", err)
	}
	if ids := idsOf(got); ids != builtinIDs() {
		t.Fatalf("models = %q, want the built-in catalogue", ids)
	}
	if n := stub.count(); n != 1 {
		t.Fatalf("upstream saw %d requests, want 1", n)
	}

	// A refusal is remembered for the TTL: the panel's own refresh loop must
	// not hammer an upstream that just said no.
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if n := stub.count(); n != 1 {
		t.Fatalf("upstream saw %d requests after a second Models call, want 1", n)
	}
}

func TestModelsMalformedBodyKeepsCatalogue(t *testing.T) {
	bodies := []string{
		`{"data":`,                     // truncated JSON
		`not json at all`,              // not JSON
		``,                             // empty body
		`{}`,                           // no list at all
		`{"data":[]}`,                  // empty list
		`{"data":null}`,                // explicit null
		`{"data":[{"no_id":true}]}`,    // entries without ids
		`{"data":[{"id":"   "}]}`,      // blank id
		`{"data":{"object":"list"}}`,   // an object where a list belongs
		`{"data":{"kimi-k2":{"x":1}}}`, // ditto: keys must not become models
		`{"data":"kimi-k2"}`,           // a scalar
		`{"error":{"message":"boom"}}`, // an error envelope on a 200
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			stub := newModelStub(t, func() (int, string) { return http.StatusOK, body })
			c := modelsClient(t, map[string]any{"api_base": stub.srv.URL}, "tok-malformed-98765")

			got, err := c.Models(context.Background())
			if err != nil {
				t.Fatalf("Models must not fail on a malformed body: %v", err)
			}
			if len(got) == 0 {
				t.Fatal("a malformed body emptied the catalogue")
			}
			if ids := idsOf(got); ids != builtinIDs() {
				t.Fatalf("models = %q, want the built-in catalogue", ids)
			}

			// The explicit path reports the failure, and still answers.
			refreshed, rerr := c.RefreshModels(context.Background())
			if rerr == nil {
				t.Fatal("RefreshModels on a malformed body should report an error")
			}
			if !strings.HasPrefix(rerr.Error(), "kimi: ") {
				t.Errorf("error = %q, want the kimi: prefix", rerr.Error())
			}
			if len(refreshed) == 0 {
				t.Fatal("RefreshModels emptied the catalogue")
			}
		})
	}
}

func TestRefreshModelsFailureKeepsLastGoodList(t *testing.T) {
	var failing atomic.Bool
	stub := newModelStub(t, func() (int, string) {
		if failing.Load() {
			return http.StatusInternalServerError, `{"error":{"message":"upstream is unwell"}}`
		}
		return http.StatusOK, `{"data":[{"id":"kimi-k2.5"},{"id":"kimi-k3"}]}`
	})
	c := modelsClient(t, map[string]any{"api_base": stub.srv.URL}, "tok-refresh-246810")

	first, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if ids := idsOf(first); ids != "kimi-k2.5,kimi-k3" {
		t.Fatalf("models = %q, want the upstream ids", ids)
	}

	failing.Store(true)
	got, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("a failing refresh must report an error")
	}
	if !strings.HasPrefix(err.Error(), "kimi: ") {
		t.Errorf("error = %q, want the kimi: prefix", err.Error())
	}
	if ids := idsOf(got); ids != "kimi-k2.5,kimi-k3" {
		t.Fatalf("a failed refresh returned %q, want the previous good list", ids)
	}

	// And the cheap path still serves the good list rather than the baseline.
	cheap, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if ids := idsOf(cheap); ids != "kimi-k2.5,kimi-k3" {
		t.Fatalf("Models = %q, want the last good list", ids)
	}
}

func TestRefreshModelsFirstFailureReturnsBaselineWithError(t *testing.T) {
	stub := newModelStub(t, func() (int, string) {
		return http.StatusBadGateway, `{"error":{"message":"gateway"}}`
	})
	c := modelsClient(t, map[string]any{"api_base": stub.srv.URL}, "tok-firstfail-13579")

	got, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("a failed first refresh must report an error")
	}
	if ids := idsOf(got); ids != builtinIDs() {
		t.Fatalf("models = %q, want the built-in catalogue", ids)
	}
}

func TestRefreshModelsEmptyUpstreamListKeepsLastGood(t *testing.T) {
	var empty atomic.Bool
	stub := newModelStub(t, func() (int, string) {
		if empty.Load() {
			return http.StatusOK, `{"data":[]}`
		}
		return http.StatusOK, `{"data":[{"id":"kimi-k2.5"}]}`
	})
	c := modelsClient(t, map[string]any{"api_base": stub.srv.URL}, "tok-empty-11223344")

	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	empty.Store(true)
	got, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("a 200 with an empty list should be reported as a failure")
	}
	if ids := idsOf(got); ids != "kimi-k2.5" {
		t.Fatalf("models = %q, want the previous good list", ids)
	}
}

// ---------------------------------------------------------------------------
// Caching
// ---------------------------------------------------------------------------

func TestModelsSecondCallWithinTTLUsesCache(t *testing.T) {
	stub := newModelStub(t, func() (int, string) {
		return http.StatusOK, `{"data":[{"id":"kimi-k2.5"}]}`
	})
	c := modelsClient(t, map[string]any{"api_base": stub.srv.URL}, "tok-ttl-5566778899")

	for i := 0; i < 3; i++ {
		got, err := c.Models(context.Background())
		if err != nil {
			t.Fatalf("Models #%d: %v", i, err)
		}
		if ids := idsOf(got); ids != "kimi-k2.5" {
			t.Fatalf("Models #%d = %q", i, ids)
		}
	}
	if n := stub.count(); n != 1 {
		t.Fatalf("upstream saw %d requests for 3 Models calls, want 1 (the TTL cache)", n)
	}

	// The panel's explicit button bypasses the TTL.
	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if n := stub.count(); n != 2 {
		t.Fatalf("upstream saw %d requests after RefreshModels, want 2", n)
	}
}

func TestModelsReturnsACopy(t *testing.T) {
	stub := newModelStub(t, func() (int, string) {
		return http.StatusOK, `{"data":[{"id":"kimi-k2.5","context_length":262144}]}`
	})
	c := modelsClient(t, map[string]any{"api_base": stub.srv.URL}, "tok-copy-99887766")

	first, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	first[0].ID = "mutated"
	first[0].Extra["context_length"] = 1
	first[0].Extra["injected"] = true

	again, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if again[0].ID != "kimi-k2.5" {
		t.Fatalf("a caller mutated the cached catalogue: id = %q", again[0].ID)
	}
	if v := again[0].Extra["context_length"]; v != 262144 {
		t.Fatalf("a caller mutated the cached Extra map: context_length = %v", v)
	}
	if _, ok := again[0].Extra["injected"]; ok {
		t.Fatal("a caller mutated the cached Extra map")
	}
}

// ---------------------------------------------------------------------------
// Token sources
// ---------------------------------------------------------------------------

func TestModelsUsesCredentialFileToken(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "kimi-code.json")
	// expires_at is in 1970: an expiry this module cannot verify must not
	// discard a token the vendor would still accept.
	body := `{"access_token":"cli-file-access-token-31337","refresh_token":"r","expires_at":1,"scope":"x","token_type":"Bearer"}`
	if err := os.WriteFile(credFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	stub := newModelStub(t, func() (int, string) {
		return http.StatusOK, `{"data":[{"id":"kimi-k2.5"}]}`
	})
	c := modelsClient(t, map[string]any{
		"api_base":         stub.srv.URL,
		"credential_files": []string{credFile},
	}, "")

	got, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if ids := idsOf(got); ids != "kimi-k2.5" {
		t.Fatalf("models = %q, want the upstream ids", ids)
	}
	if a := stub.lastAuth(); a != "Bearer cli-file-access-token-31337" {
		t.Errorf("Authorization = %q, want the token from the credential file", a)
	}
}

func TestModelsStoredTokenBeatsCredentialFile(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "kimi-code.json")
	if err := os.WriteFile(credFile, []byte(`{"access_token":"cli-file-token-abcdef"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := newModelStub(t, func() (int, string) {
		return http.StatusOK, `{"data":[{"id":"kimi-k2.5"}]}`
	})
	c := modelsClient(t, map[string]any{
		"api_base":         stub.srv.URL,
		"credential_files": []string{credFile},
	}, "panel-store-token-abcdef")

	if _, err := c.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if a := stub.lastAuth(); a != "Bearer panel-store-token-abcdef" {
		t.Errorf("Authorization = %q, want the module's own stored token", a)
	}
}

// ---------------------------------------------------------------------------
// Secrets
// ---------------------------------------------------------------------------

func TestModelsTokenNeverLeaksIntoAnError(t *testing.T) {
	cases := []struct {
		name  string
		token string
		body  string
	}{
		{
			name:  "opaque token echoed by the vendor",
			token: "opaque-access-token-9f3a2b7c",
			body:  `{"error":{"message":"invalid token opaque-access-token-9f3a2b7c"}}`,
		},
		{
			name:  "JWT echoed by the vendor",
			token: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
			body:  `{"error":{"message":"bad jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"}}`,
		},
		{
			name:  "bearer echo",
			token: "another-panel-token-4d5e6f",
			body:  `{"error":{"message":"Bearer another-panel-token-4d5e6f is not valid"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newModelStub(t, func() (int, string) {
				return http.StatusInternalServerError, tc.body
			})
			c := modelsClient(t, map[string]any{"api_base": stub.srv.URL}, tc.token)

			_, err := c.RefreshModels(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.HasPrefix(err.Error(), "kimi: ") {
				t.Errorf("error = %q, want the kimi: prefix", err.Error())
			}
			if strings.Contains(err.Error(), tc.token) {
				t.Fatalf("the token survived into the error: %q", err.Error())
			}

			// The cheap path logs the same error, so it must be scrubbed too.
			if _, err := c.Models(context.Background()); err != nil {
				t.Fatalf("Models must not fail here: %v", err)
			}
			if last := c.lastError(); strings.Contains(last, tc.token) {
				t.Fatalf("the token survived into the logged error: %q", last)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

func TestModelsConcurrentUse(t *testing.T) {
	stub := newModelStub(t, func() (int, string) {
		time.Sleep(10 * time.Millisecond) // widen the race window
		return http.StatusOK, `{"data":[{"id":"kimi-k2.5"}]}`
	})
	c := modelsClient(t, map[string]any{"api_base": stub.srv.URL}, "tok-concurrent-77777")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				got, err := c.Models(context.Background())
				if err != nil {
					t.Errorf("Models: %v", err)
					return
				}
				if len(got) == 0 {
					t.Error("concurrent Models returned an empty catalogue")
					return
				}
			}
		}()
	}
	wg.Wait()

	if n := stub.count(); n != 1 {
		t.Errorf("upstream saw %d requests from 8 concurrent callers, want 1", n)
	}
	if got, err := c.Models(context.Background()); err != nil || idsOf(got) != "kimi-k2.5" {
		t.Fatalf("after the concurrent burst: %q, %v", idsOf(got), err)
	}
}

func TestRefreshModelsConcurrentWithModels(t *testing.T) {
	stub := newModelStub(t, func() (int, string) {
		return http.StatusOK, `{"data":[{"id":"kimi-k2.5"},{"id":"kimi-k3"}]}`
	})
	c := modelsClient(t, map[string]any{"api_base": stub.srv.URL}, "tok-mixed-31415926")

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			var got []core.Model
			if i%2 == 0 {
				got, err = c.RefreshModels(context.Background())
			} else {
				got, err = c.Models(context.Background())
			}
			if err != nil {
				t.Errorf("call %d: %v", i, err)
				return
			}
			if len(got) == 0 {
				t.Errorf("call %d returned an empty catalogue", i)
			}
		}(i)
	}
	wg.Wait()
}
