package openaicompat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

// A relay with no billing surface answers every "auto" probe with a 404.  The
// sweep runs every five minutes, so without a negative cache one such platform
// produced four failed requests per sweep, forever -- the bulk of the
// per-platform error log an operator reads as "everything is broken".
func TestAutoBalanceStopsProbingARelayThatHasNoBillingSurface(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	s, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL + "/v1"}, core.Deps{
		DataDir:    t.TempDir(),
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	acct, err := s.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"api_key": "key-a"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.AccountBalance(context.Background(), acct.ID, 0); err == nil {
		t.Fatal("a relay with no billing surface must report the probe failure")
	}
	first := atomic.LoadInt64(&hits)
	if first == 0 {
		t.Fatal("the first probe must actually go out")
	}

	// The second and third sweeps must not touch the network at all.
	for i := 0; i < 2; i++ {
		if _, err := s.AccountBalance(context.Background(), acct.ID, 0); err == nil {
			t.Fatal("a cached negative verdict must still be reported as a failure")
		}
	}
	if got := atomic.LoadInt64(&hits); got != first {
		t.Fatalf("probe requests = %d after three sweeps, want %d (the negative cache was ignored)", got, first)
	}

	// Once the window lapses the relay is asked again.
	base := time.Now()
	s.now = func() time.Time { return base.Add(balanceProbeTTL + time.Second) }
	if _, err := s.AccountBalance(context.Background(), acct.ID, 0); err == nil {
		t.Fatal("a lapsed negative cache must still report the failure")
	}
	if got := atomic.LoadInt64(&hits); got <= first {
		t.Fatalf("probe requests = %d after the window lapsed, want more than %d", got, first)
	}
}

// A 404 from a billing endpoint says nothing about the credential, so it must
// not park the account.  A rejected AppKey says everything, so it must.
func TestBalanceProbeOnlyParksARejectedCredential(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantParked bool
	}{
		{
			name:       "a missing billing endpoint leaves the account alone",
			status:     http.StatusNotFound,
			body:       `{"error_msg":"404 Route Not Found"}`,
			wantParked: false,
		},
		{
			name:       "an HTML page where JSON was expected leaves the account alone",
			status:     http.StatusOK,
			body:       `<html>hello</html>`,
			wantParked: false,
		},
		{
			name:       "a 5xx leaves the account alone",
			status:     http.StatusBadGateway,
			body:       `bad gateway`,
			wantParked: false,
		},
		{
			name:       "an invalid AppKey is a dead credential",
			status:     http.StatusUnauthorized,
			body:       `{"error_msg":"AppKey or AppSecret is invalid","error_code":"APIG.1009"}`,
			wantParked: true,
		},
		{
			name:       "a banned key is a dead credential",
			status:     http.StatusForbidden,
			body:       `User has been banned`,
			wantParked: true,
		},
		{
			name:       "an ambiguous 403 is not",
			status:     http.StatusForbidden,
			body:       `Forbidden`,
			wantParked: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			s, err := newSource("relay", core.SourceConfig{
				BaseURL:         srv.URL + "/v1",
				BalanceProtocol: "newapi",
			}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			acct, err := s.AddAccount(context.Background(), core.AccountSpec{
				Fields: map[string]string{"api_key": "key-a"},
			})
			if err != nil {
				t.Fatal(err)
			}

			_, _ = s.AccountBalance(context.Background(), acct.ID, 0)

			var rec core.AccountRecord
			found := false
			for _, r := range s.pool.records(s.now()) {
				if strings.EqualFold(r.ID, acct.ID) {
					rec, found = r, true
					break
				}
			}
			if !found {
				t.Fatal("account vanished")
			}
			parked := !strings.EqualFold(rec.State, "ready")
			if parked != tc.wantParked {
				t.Fatalf("state after probe = %q (parked=%v), want parked=%v; note=%q",
					rec.State, parked, tc.wantParked, rec.Note)
			}
		})
	}
}
