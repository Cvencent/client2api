package tabbit

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"client2api/internal/core"
)

// checkin_test.go drives the Tabbit daily sign-in (每日签到) against a fake vendor.
//
// The endpoint pair was read off the vendor's own web bundle, not guessed:
//
//	POST /api/commerce/activity/v1/sign-in          body: the scene being signed in for
//	GET  /api/commerce/activity/v1/sign-in/status   ?scene_codes=<scene>
//
// The bundle's API client defaults the status scene to "desktop_pet", and the
// reward is credited as usage ("Sign-in Reward" in the vendor's own i18n table).
// Tabbit's quota endpoint reports a percentage, so a 3% daily grant shows up as
// usage_percentage dropping — which is what the panel's balance column renders.

const (
	webTestSignInPath   = "/api/commerce/activity/v1/sign-in"
	webTestSignInStatus = webTestSignInPath + "/status"
)

// The reply struct itself lives in checkin.go; these tests drive the module's
// own decoding of it.

// fakeSignIn wires a vendor that answers both sign-in endpoints.  granted
// controls whether the POST reports a reward, so one helper covers both the
// "credited" and the "already done today" answers.
func fakeSignIn(t *testing.T, granted bool, statusBody string) (*fakeWeb, *signInState) {
	t.Helper()
	f := newFakeWeb(t)
	st := &signInState{granted: granted}
	mux := http.NewServeMux()
	mux.HandleFunc(webTestSignInPath, func(w http.ResponseWriter, r *http.Request) {
		f.note("signin")
		if c := r.Header.Get("Cookie"); !strings.HasPrefix(c, "token=") {
			t.Errorf("the sign-in call carried no token cookie (Cookie=%q)", c)
			http.Error(w, "no cookie", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("sign-in used %s, want POST", r.Method)
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		raw := make([]byte, 4096)
		n, _ := r.Body.Read(raw)
		var body map[string]any
		if err := json.Unmarshal(raw[:n], &body); err != nil {
			t.Errorf("the sign-in body is not JSON: %v (%s)", err, raw[:n])
		}
		st.mu.Lock()
		st.body = body
		st.method = r.Method
		granted := st.granted
		st.mu.Unlock()
		writeWebJSON(w, `{"success":true,"granted":`+boolStr(granted)+`,"rewarded":3,"scene_code":"desktop_pet","streak":4,"message":"ok"}`)
	})
	mux.HandleFunc(webTestSignInStatus, func(w http.ResponseWriter, r *http.Request) {
		f.note("signin-status")
		if c := r.Header.Get("Cookie"); !strings.HasPrefix(c, "token=") {
			t.Errorf("the sign-in status call carried no token cookie (Cookie=%q)", c)
			http.Error(w, "no cookie", http.StatusUnauthorized)
			return
		}
		if got := r.URL.Query()["scene_codes"]; len(got) != 1 || got[0] != webSignInScene {
			t.Errorf("the status was asked for scene_codes=%v, want [%s]", got, webSignInScene)
		}
		body := statusBody
		if body == "" {
			body = `{"signed_in":false,"streak":3}`
		}
		writeWebJSON(w, body)
	})
	f.Server.Config.Handler = mux
	return f, st
}

type signInState struct {
	mu      sync.Mutex
	granted bool
	method  string
	body    map[string]any
}

func (s *signInState) snapshot() (string, map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.method, s.body
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestTabbitCheckinClaimsTheDailyReward is the happy path: the panel button must
// POST the vendor's sign-in endpoint and report the reward as a success.
func TestTabbitCheckinClaimsTheDailyReward(t *testing.T) {
	f, _ := fakeSignIn(t, true, "")
	c, rec := newWebClient(t, f)

	res, err := c.Checkin(context.Background(), rec.ID, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("Checkin reported not OK: %+v", res)
	}
	if n := f.hits("signin"); n != 1 {
		t.Fatalf("the sign-in endpoint was hit %d times, want 1", n)
	}
	if res.Action != webSignInScene {
		t.Errorf("Action = %q, want %q", res.Action, webSignInScene)
	}
}

// TestTabbitCheckinSendsTheSceneCode locks the request body: the vendor's own
// client names the scene it is signing in for, and the module must send the same
// one or the vendor credits the wrong thing.
func TestTabbitCheckinSendsTheSceneCode(t *testing.T) {
	f, st := fakeSignIn(t, true, "")
	c, rec := newWebClient(t, f)

	if _, err := c.Checkin(context.Background(), rec.ID, ""); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	method, body := st.snapshot()
	if method != http.MethodPost {
		t.Fatalf("sign-in used %s, want POST", method)
	}
	if got, _ := body["scene_code"].(string); got != webSignInScene {
		t.Fatalf("the body carried scene_code=%q, want %q (body=%v)", got, webSignInScene, body)
	}
}

// TestTabbitCheckinReportsAAlreadyClaimedDayAsARefusal is the important one: a
// vendor that declines is a RESULT, not an error.  Reporting it as OK would tell
// the operator they got 3% when they got nothing, and the balance column would
// then be quietly wrong.
func TestTabbitCheckinReportsAnAlreadyClaimedDayAsARefusal(t *testing.T) {
	f, _ := fakeSignIn(t, false, "")
	c, rec := newWebClient(t, f)

	res, err := c.Checkin(context.Background(), rec.ID, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatalf("a declined sign-in was reported as OK: %+v", res)
	}
	if res.Error == "" {
		t.Error("a declined sign-in carried no explanation")
	}
	if res.ElapsedMS < 0 {
		t.Errorf("ElapsedMS = %d", res.ElapsedMS)
	}
}

// TestTabbitCheckinRejectsAnUnknownAction guards the action parameter: the panel
// posts an empty string for "the default action", and anything else must be
// refused rather than silently claimed.
func TestTabbitCheckinRejectsAnUnknownAction(t *testing.T) {
	f, _ := fakeSignIn(t, true, "")
	c, rec := newWebClient(t, f)

	res, err := c.Checkin(context.Background(), rec.ID, "not-a-scene")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK || !strings.Contains(res.Error, "not-a-scene") {
		t.Fatalf("an unknown action was not refused: %+v", res)
	}
	if n := f.hits("signin"); n != 0 {
		t.Fatalf("an unknown action still reached the vendor %d time(s)", n)
	}
}

// TestTabbitCheckinRefusesASidecarAccount: only web-token accounts have a vendor
// session to sign in with.  A sidecar account is a local bridge, and asking the
// vendor for it would be a guaranteed 401.
func TestTabbitCheckinRefusesASidecarAccount(t *testing.T) {
	f := newFakeWeb(t)
	c, _ := newWebClient(t, f)

	sidecar, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{
			"kind":     "sidecar",
			"base_url": f.URL,
		},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	res, err := c.Checkin(context.Background(), sidecar.ID, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatalf("a sidecar account was reported as signed in: %+v", res)
	}
	if n := f.hits("signin"); n != 0 {
		t.Fatalf("a sidecar sign-in still reached the vendor %d time(s)", n)
	}
}

// TestTabbitCheckinRelaysARejectedSession: a 401 from the vendor means the
// cookie died, and the message must say so — the operator's fix is to press
// 导入凭据, which the shared webError text already spells out.
func TestTabbitCheckinRelaysARejectedSession(t *testing.T) {
	f := newFakeWeb(t)
	mux := http.NewServeMux()
	mux.HandleFunc(webTestSignInPath, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"token expired"}`, http.StatusUnauthorized)
	})
	f.Server.Config.Handler = mux
	c, rec := newWebClient(t, f)

	res, err := c.Checkin(context.Background(), rec.ID, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatal("a 401 was reported as a successful sign-in")
	}
	if !strings.Contains(res.Error, "导入凭据") {
		t.Errorf("the 401 message does not point at the fix: %q", res.Error)
	}
}

// TestTabbitCheckinStatusReadsTheStreak proves the status endpoint is wired the
// way the vendor's client calls it: GET, one scene_codes value.
func TestTabbitCheckinStatusReadsTheStreak(t *testing.T) {
	f := newFakeWeb(t)
	mux := http.NewServeMux()
	mux.HandleFunc(webTestSignInStatus, func(w http.ResponseWriter, r *http.Request) {
		f.note("signin-status")
		if got := r.URL.Query()["scene_codes"]; len(got) != 1 || got[0] != webSignInScene {
			t.Errorf("scene_codes=%v, want [%s]", got, webSignInScene)
		}
		writeWebJSON(w, `{"signed_in":true,"streak":7,"rewarded":3}`)
	})
	f.Server.Config.Handler = mux
	c, _ := newWebClient(t, f)

	got, err := c.webSignInStatus(context.Background(), c.webAuthFrom("tok", "lbl", epOriginPanel, f.URL))
	if err != nil {
		t.Fatalf("webSignInStatus: %v", err)
	}
	if !got.SignedIn || got.Streak != 7 {
		t.Fatalf("status = %+v, want signed-in with a 7-day streak", got)
	}
}

// TestTabbitCheckinActionsAppearOnlyWithAnAccount: a button that cannot work is a
// lie the operator pays for with a failed request, so no account means no action.
func TestTabbitCheckinActionsAppearOnlyWithAnAccount(t *testing.T) {
	f, _ := fakeSignIn(t, true, "")
	c, _ := newWebClient(t, f)
	acts := c.CheckinActions(context.Background())
	if len(acts) != 1 {
		t.Fatalf("CheckinActions = %+v, want exactly one", acts)
	}
	if acts[0].ID != webSignInScene || acts[0].Label == "" {
		t.Fatalf("the action is %+v, want id %q with a label", acts[0], webSignInScene)
	}
	// The panel renders this label verbatim, so it has to be the operator's
	// language, not the vendor's.
	if !isChineseLabel(acts[0].Label) {
		t.Errorf("the action label %q is not Chinese", acts[0].Label)
	}
}

func isChineseLabel(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}

// TestWebSignInStatusAcceptsStringNumbers mirrors the quota endpoint's habit of
// sending numbers as strings; a status that says "3" must not fail to parse.
func TestWebSignInStatusAcceptsStringNumbers(t *testing.T) {
	var st webSignInStatus
	raw := `{"signed_in":"true","streak":"7","rewarded":"3"}`
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !st.SignedIn || st.Streak != 7 || st.Rewarded != 3 {
		t.Fatalf("parsed = %+v", st)
	}
}

// TestSignInReplyToleratesAMissingReward keeps a thin reply from becoming an
// error: the panel only needs to know whether anything moved.
func TestSignInReplyToleratesAMissingReward(t *testing.T) {
	var r signInReply
	if err := json.Unmarshal([]byte(`{"success":true,"granted":true}`), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !r.Success || !r.Granted {
		t.Fatalf("parsed = %+v", r)
	}
}
