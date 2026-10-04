package panel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/gateway"
)

// ---------------------------------------------------------------------------
// The route surface.
//
// The complaint this file exists for is "looks done but isn't": a management
// route written, unit-tested through its own handler, and then never
// registered.  A handler test cannot see that, because it calls the handler
// directly -- only a request that goes through New's mux can tell "written"
// apart from "wired".
//
// The signature of a route that is not registered is the shell's plain-text
// 404 (http.NotFound in New's /panel/ fallback).  A handler's own 404 is JSON
// with our {"ok":false,...} shape, so the two are distinguishable and this
// test can insist on the second without pinning every handler's status code.
// ---------------------------------------------------------------------------

// The signature of a route that is not registered depends on where it lives,
// because two different fallbacks answer for it:
//
//   - a top-level /panel/api/... path nobody registered falls through to the
//     shell in New (http.NotFound) and comes back as plain text;
//   - anything under /panel/api/clients/ reaches handleClientScoped, which
//     answers for its own unknown sub-paths with JSON (panel.go:437).
//
// Both are "no handler of yours wanted this".  A handler's own 404 is JSON with
// our {"ok":false,...} shape and neither wording, so the routes below can be
// required to reach a handler without pinning every status code they choose.
const (
	missPlain  = "404 page not found"
	missScoped = "no such panel endpoint"
)

// routeMiss returns the wording of a response that no route claimed, or "" when
// the request did reach a handler.
func routeMiss(body string) string {
	trimmed := strings.TrimSpace(body)
	switch {
	case strings.HasPrefix(trimmed, missPlain):
		return missPlain
	case strings.Contains(trimmed, missScoped):
		return missScoped
	}
	return ""
}

func TestEveryManagementRouteIsReachable(t *testing.T) {
	fastAutoGap(t)

	// Between them the two fakes answer every capability the panel knows how to
	// ask about, so a non-404 below means the route reached a handler and the
	// handler got far enough to talk to its module.
	verbs := verbClient("verbs", []core.TaskInfo{{Code: "t1", Desc: "每日签到", Auto: true}},
		core.AccountRecord{ID: "A1", Label: "一号"})
	quota := quotaClient("quota", core.AccountRecord{ID: "A1", Label: "一号"})

	h := New(Options{
		Registry: registryOf(verbs, quota),
		Version:  "test",
		Listen:   "127.0.0.1:0",
		Started:  time.Now(),
		Stats:    gateway.NewStats(),
		Logs:     gateway.NewLogRing(8),
	})

	base := "/panel/api"
	routes := []struct {
		method, path, body string
	}{
		// top level
		{http.MethodGet, base + "/status", ""},
		{http.MethodPost, base + "/reload", ""},
		{http.MethodGet, base + "/clients", ""},
		{http.MethodGet, base + "/overview", ""},
		{http.MethodGet, base + "/logs", ""},
		{http.MethodGet, base + "/models", ""},
		{http.MethodPost, base + "/models/refresh", ""},
		{http.MethodGet, base + "/usage", ""},
		{http.MethodPost, base + "/usage/save", ""},
		{http.MethodGet, base + "/config", ""},
		{http.MethodGet, base + "/model_probes", ""},

		// per client, hand-dispatched inside handleClientScoped
		{http.MethodGet, base + "/clients/verbs/capabilities", ""},
		{http.MethodGet, base + "/clients/verbs/accounts", ""},
		{http.MethodPost, base + "/clients/verbs/accounts/refresh", ""},
		{http.MethodPost, base + "/clients/verbs/discover", ""},
		{http.MethodPost, base + "/clients/verbs/import", ""},
		{http.MethodPost, base + "/clients/verbs/login", ""},
		{http.MethodGet, base + "/clients/verbs/login/regions", ""},
		{http.MethodGet, base + "/clients/verbs/login/sess-1", ""},

		// the board and its run log
		{http.MethodGet, base + "/clients/verbs/tasks", ""},
		{http.MethodPost, base + "/clients/verbs/tasks/t1/run", ""},
		{http.MethodGet, base + "/clients/verbs/tasks/runs/nope", ""},

		// the task queue
		{http.MethodPost, base + "/clients/verbs/tasks/scan_all", ""},
		{http.MethodGet, base + "/clients/verbs/tasks/queue", ""},
		{http.MethodPost, base + "/clients/verbs/tasks/run_queue", ""},

		// the batch surface and the reference's one-click aliases
		{http.MethodGet, base + "/clients/verbs/batches", ""},
		{http.MethodGet, base + "/clients/verbs/batches/queue", ""},
		{http.MethodGet, base + "/clients/verbs/batches/runs/nope", ""},
		{http.MethodPost, base + "/clients/verbs/batches/checkin/run", ""},
		{http.MethodPost, base + "/clients/verbs/batches/checkin_all/run", ""},

		// one account
		{http.MethodGet, base + "/clients/verbs/accounts/A1", ""},
		{http.MethodPost, base + "/clients/verbs/accounts/A1/test", ""},
		{http.MethodPost, base + "/clients/verbs/accounts/A1/enabled", `{"enabled":true}`},
		{http.MethodPost, base + "/clients/verbs/accounts/A1/checkin", ""},
		{http.MethodPost, base + "/clients/verbs/accounts/A1/revive", ""},
		{http.MethodPost, base + "/clients/verbs/accounts/A1/balance", ""},

		// the per-account task verbs
		{http.MethodGet, base + "/clients/verbs/accounts/A1/tasks", ""},
		{http.MethodPost, base + "/clients/verbs/accounts/A1/tasks/accept", `{"task_codes":["t1"]}`},
		{http.MethodPost, base + "/clients/verbs/accounts/A1/tasks/accept_all", ""},
		{http.MethodPost, base + "/clients/verbs/accounts/A1/tasks/claim", `{"task_code":"t1"}`},
		{http.MethodPost, base + "/clients/verbs/accounts/A1/tasks/auto", `{"task_code":"t1"}`},
		{http.MethodPost, base + "/clients/verbs/accounts/A1/tasks/auto_all", ""},

		// credits, packages and vouchers
		{http.MethodPost, base + "/clients/quota/accounts/A1/balance", ""},
		{http.MethodGet, base + "/clients/quota/packages", ""},
		{http.MethodGet, base + "/clients/quota/school/vouchers", ""},

		// conversation stickiness, which is how the chat tab aims a request at
		// one chosen account
		{http.MethodGet, base + "/clients/verbs/conversations?key=k1", ""},
		{http.MethodPost, base + "/clients/verbs/conversations", `{"account":"A1"}`},
		{http.MethodPost, base + "/clients/verbs/conversations/unbind", `{"key":"k1"}`},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+strings.TrimPrefix(rt.path, base), func(t *testing.T) {
			rec := hitRoute(t, h, rt.method, rt.path, rt.body)
			if miss := routeMiss(rec.Body.String()); miss != "" {
				t.Fatalf("%s %s reached no handler (%q, status %d): register it in "+
					"internal/panel/panel.go or handleClientScoped; body=%s",
					rt.method, rt.path, miss, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestUnregisteredPathsLookLikeAMiss pins the two discriminators the test above
// keys on.  It is the other half of that test: if a fallback ever started
// answering JSON of its own -- or handleClientScoped's default branch stopped
// naming the path -- the route-surface check would silently pass for every
// route it is supposed to protect.
func TestUnregisteredPathsLookLikeAMiss(t *testing.T) {
	h := New(Options{Registry: registryOf(&fakeClient{name: "plain"}), Started: time.Now()})

	for _, tc := range []struct {
		path, want string
	}{
		// top level: nothing under /panel/api/ claims it, so the shell does.
		{"/panel/api/there-is-no-such-endpoint", missPlain},
		// under /clients/: handleClientScoped owns the prefix and answers for
		// its own unknown sub-paths.
		{"/panel/api/clients/plain/not-a-route", missScoped},
		{"/panel/api/clients/plain/accounts/A1/not-a-verb", missScoped},
	} {
		rec := hitRoute(t, h, http.MethodGet, tc.path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", tc.path, rec.Code)
		}
		if got := routeMiss(rec.Body.String()); got != tc.want {
			t.Errorf("GET %s = %q, want the %q wording", tc.path, rec.Body.String(), tc.want)
		}
	}
}

func hitRoute(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
