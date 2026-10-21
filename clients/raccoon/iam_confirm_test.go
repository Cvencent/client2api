package raccoon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"client2api/internal/core"
)

// confirmUpstream models the vendor's "business session not confirmed" gate.
//
// The web bundle's own client posts /api/web/auth/v1/iam/confirm and then
// retries whatever call was refused with 200901. Without that step a brand new
// IAM sign-in looks like a broke, empty account: balance answers
// iam_confirm_required and user_info answers nothing at all.
type confirmUpstream struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	paths   []string
	confirm int
	balance int
}

func newConfirmUpstream(t *testing.T) *confirmUpstream {
	t.Helper()
	u := &confirmUpstream{t: t}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<16))
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		confirmed := u.confirm > 0
		u.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case pathIAMConfirm:
			if got := r.Header.Get("Authorization"); got != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"code":200001,"message":"unauthorized"}`)
				return
			}
			u.mu.Lock()
			u.confirm++
			u.mu.Unlock()
			_, _ = io.WriteString(w, `{"code":0,"message":"success"}`)
		case pathUserInfo:
			if !confirmed {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"code":200901,"message":"iam_confirm_required","details":"IAM confirmation required"}`)
				return
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"id":"7590634","name":"RaccoonEthan","phone":"19937167593"}}`)
		case pathPointsGrant:
			_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
		case pathBalance:
			u.mu.Lock()
			u.balance++
			n := u.balance
			u.mu.Unlock()
			if n == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"code":200901,"message":"iam_confirm_required","details":"IAM confirmation required"}`)
				return
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"available_points":11398}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "404 page not found\n")
		}
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *confirmUpstream) countPath(p string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, got := range u.paths {
		if got == p {
			n++
		}
	}
	return n
}

func (u *confirmUpstream) indexOf(p string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, got := range u.paths {
		if got == p {
			return i
		}
	}
	return -1
}

// A fresh IAM sign-in must confirm the business session BEFORE it reads the
// profile, otherwise the profile read is refused with 200901 and the account
// is stored as an anonymous bare token.
func TestFreshLoginConfirmsTheBusinessSessionFirst(t *testing.T) {
	u := newConfirmUpstream(t)
	c := newTestClient(t, t.TempDir(), `{"base_url":`+strconvQuote(u.server.URL)+`}`, nil)

	e := c.storeCredential(context.Background(), credential{AccessToken: "tok"})
	if e == nil {
		t.Fatal("storeCredential returned no entry")
	}

	if u.countPath(pathIAMConfirm) == 0 {
		t.Fatal("the login never confirmed the business session; every business call will answer 200901")
	}
	ci, ui := u.indexOf(pathIAMConfirm), u.indexOf(pathUserInfo)
	if ci < 0 || ui < 0 {
		t.Fatalf("paths = %v, want both confirm and user_info", u.paths)
	}
	if ci > ui {
		t.Fatalf("confirm at %d ran AFTER user_info at %d; the profile read is refused before confirmation", ci, ui)
	}
	if e.acct.cred().UserID != "7590634" {
		t.Fatalf("UserID = %q, want the confirmed profile id 7590634", e.acct.cred().UserID)
	}
	if got := e.acct.cred().Nickname; got != "RaccoonEthan" {
		t.Fatalf("Nickname = %q, want RaccoonEthan", got)
	}
}

// A business call that meets 200901 must confirm the session and retry once,
// so an account stored before this fix heals itself instead of staying dead.
func TestBusinessCallConfirmsAndRetriesOn200901(t *testing.T) {
	u := newConfirmUpstream(t)
	c := newTestClient(t, t.TempDir(), `{"base_url":`+strconvQuote(u.server.URL)+`}`, nil)

	hdr := raccoonHeaders(credential{AccessToken: "tok"}, "", "")
	var out struct {
		AvailablePoints *flexFloat `json:"available_points"`
	}
	if err := c.do(context.Background(), http.MethodGet, pathBalance, hdr, nil, &out); err != nil {
		t.Fatalf("do(balance) = %v, want a confirm-and-retry success", err)
	}

	if u.countPath(pathIAMConfirm) != 1 {
		t.Fatalf("confirm calls = %d, want exactly 1", u.countPath(pathIAMConfirm))
	}
	if u.countPath(pathBalance) != 2 {
		t.Fatalf("balance calls = %d, want 2 (refused, then retried)", u.countPath(pathBalance))
	}
	if out.AvailablePoints == nil || int64(*out.AvailablePoints) != 11398 {
		t.Fatalf("available_points = %v, want 11398", out.AvailablePoints)
	}
}

// The confirm call must never recurse: a refusal from the confirm endpoint
// itself has to surface as the original error, not loop forever.
func TestConfirmEndpointFailureDoesNotRecurse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":200901,"message":"iam_confirm_required"}`)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, t.TempDir(), `{"base_url":`+strconvQuote(srv.URL)+`}`, nil)

	hdr := raccoonHeaders(credential{AccessToken: "tok"}, "", "")
	err := c.do(context.Background(), http.MethodGet, pathBalance, hdr, nil, &struct{}{})
	if err == nil {
		t.Fatal("a persistent 200901 must surface as an error")
	}
	var ae *apiError
	if !asAPIError(err, &ae) || ae.Code != codeIAMConfirmRequired {
		t.Fatalf("error = %v, want the 200901 apiError", err)
	}
}

var _ = json.Marshal

// The chat stream must repair 200901 the same way: confirm, then replay, so a
// stored-but-unconfirmed account is not counted as a failed account on its
// first message.
func TestChatStreamConfirmsAndRetriesOn200901(t *testing.T) {
	var mu sync.Mutex
	confirm, chats := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<16))
		mu.Lock()
		confirmed := confirm > 0
		if r.URL.Path == pathIAMConfirm {
			confirm++
		}
		if r.URL.Path == pathChat {
			chats++
		}
		n := chats
		mu.Unlock()

		switch r.URL.Path {
		case pathIAMConfirm:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"message":"success"}`)
		case pathChat:
			if !confirmed && n == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"code":200901,"message":"iam_confirm_required"}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, t.TempDir(), `{"base_url":`+strconvQuote(srv.URL)+`}`, nil)

	resp, err := c.postChat(context.Background(), credential{AccessToken: "tok"},
		&core.ChatRequest{Model: "deepseek-v4.1-flash", Messages: []core.Message{{Role: "user", Content: "hi"}}},
		"deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("postChat = %v, want confirm-and-retry success", err)
	}
	defer resp.Body.Close()

	mu.Lock()
	gotConfirm, gotChats := confirm, chats
	mu.Unlock()
	if gotConfirm != 1 {
		t.Fatalf("confirm calls = %d, want 1", gotConfirm)
	}
	if gotChats != 2 {
		t.Fatalf("chat calls = %d, want 2 (refused, then retried)", gotChats)
	}
}
