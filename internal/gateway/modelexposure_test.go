package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// A 404 from the exposure whitelist used to read as "the gateway is broken",
// and the usage row carried no reason at all.  Live traffic showed the
// consequence: a client retrying an unpublished name produced a burst of
// instant 404s with no account and no duration, which looked exactly like the
// service dropping the connection.  The refusal now names which of the three
// situations it is.
func TestUnexposedModelErrorNamesTheActualCause(t *testing.T) {
	alpha := &testClient{name: "alpha", catalogue: []core.Model{{ID: "m1"}}}
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.SetExposedModels([]string{"alpha/m1"}, true)
	srv := exposedTestServer(t, reg)
	core_ := &server{opts: Options{Registry: reg}}

	t.Run("a servable but unpublished id", func(t *testing.T) {
		// Nothing is exposed under this spelling, but alpha could route the
		// bare id m1 through its catalogue.
		body := `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		srv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404: %s", rec.Code, rec.Body.String())
		}
		got := rec.Body.String()
		if !strings.Contains(got, "exposed_models") {
			t.Fatalf("refusal = %s, want it to name exposed_models as the fix", got)
		}
		if !strings.Contains(got, "served by a platform") {
			t.Fatalf("refusal = %s, want it to say the model is routable", got)
		}
		if alpha.chatCalls != 0 {
			t.Fatalf("the model reached the module %d times", alpha.chatCalls)
		}
	})

	t.Run("an id no platform serves", func(t *testing.T) {
		err := core_.unexposedModelError(context.Background(), "no-such-model-low")
		if !IsUnexposedModelError(err) {
			t.Fatalf("error = %v, want an UnexposedModelError", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, "no platform can route it") {
			t.Fatalf("refusal = %s, want it to say nothing can route the id", msg)
		}
		if strings.Contains(msg, "add it there") {
			t.Fatalf("refusal = %s, must not send the caller to a whitelist edit that would not help", msg)
		}
	})

	t.Run("the underlying routing reason is preserved", func(t *testing.T) {
		err := core_.unexposedModelError(context.Background(), "no-such-model-low")
		var target *UnexposedModelError
		if !errors.As(err, &target) {
			t.Fatalf("error = %v, want *UnexposedModelError", err)
		}
		if target.ResolveErr == nil {
			t.Fatal("ResolveErr is nil; the routing verdict must be kept for the log")
		}
		if !errors.Is(err, target.ResolveErr) {
			t.Fatal("Unwrap does not expose the routing verdict")
		}
		if target.Servable {
			t.Fatal("an unroutable id must not be reported as servable")
		}
	})
}

// The usage row must say the request never left the gateway.  Without that a
// whitelist refusal and a dropped connection produce identical rows.
func TestUnexposedModelFailureReasonBlamesThePolicy(t *testing.T) {
	alpha := &testClient{name: "alpha", catalogue: []core.Model{{ID: "m1"}}}
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.SetExposedModels([]string{"alpha/m1"}, true)
	srv := exposedTestServer(t, reg)
	core_ := &server{opts: Options{Registry: reg}}

	rec := chat(t, srv, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
	reason := failureReason(core_.unexposedModelError(context.Background(), "m1"))
	if !strings.Contains(reason, "白名单") {
		t.Fatalf("failure reason = %q, want it to name the whitelist", reason)
	}
}
