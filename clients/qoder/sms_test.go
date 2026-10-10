package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"client2api/internal/core"
)

// fakeQoderSMS is a minimal eomsg server.  The platform protocol is plain text,
// so exercising the real smscap client keeps this test honest without touching
// the network.
type fakeQoderSMS struct {
	mu       sync.Mutex
	phones   []string
	draws    int
	releases []string
	calls    []url.Values
}

func (f *fakeQoderSMS) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, q)

	switch q.Get("code") {
	case "leftAmount":
		fmt.Fprint(w, "28.55")
	case "getPhone":
		if want := q.Get("phone"); want != "" {
			fmt.Fprint(w, want)
			return
		}
		if len(f.phones) == 0 {
			fmt.Fprint(w, "ERROR no numbers")
			return
		}
		i := f.draws
		f.draws++
		if i >= len(f.phones) {
			i = len(f.phones) - 1
		}
		fmt.Fprint(w, f.phones[i])
	case "release", "block":
		f.releases = append(f.releases, q.Get("phone"))
		fmt.Fprint(w, "SUCCESS")
	default:
		fmt.Fprint(w, "ERROR unknown code")
	}
}

func qoderSMSClient(t *testing.T, srv *httptest.Server, extra string) *Client {
	t.Helper()
	if extra != "" {
		extra = "," + extra
	}
	cfg := json.RawMessage(fmt.Sprintf(
		`{"sms_token":"tok-abc","sms_base":%q%s}`,
		srv.URL, extra,
	))
	raw, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     cfg,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := raw.(*Client)
	if !ok {
		t.Fatalf("New returned %T", raw)
	}
	return c
}

func TestSMSStatusDefaultsToQoderKeyword(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	c, err := New(core.Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	st := core.CapabilitiesOf(context.Background(), c)
	if !st.SMS {
		t.Fatal("capabilities.sms = false, want true for Qoder auto-login")
	}
	status := c.(*Client).SMSStatus(context.Background(), core.SMSOpts{})
	if status.Keyword != defaultSMSKeyword {
		t.Fatalf("keyword = %q, want %q", status.Keyword, defaultSMSKeyword)
	}
}

func TestAcquirePhoneSkipsVirtualNumberSegments(t *testing.T) {
	f := &fakeQoderSMS{phones: []string{"17000000001", "13800000000"}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := qoderSMSClient(t, srv, "")

	num, err := c.AcquirePhone(context.Background(), core.SMSOpts{}, "", nil)
	if err != nil {
		t.Fatalf("AcquirePhone: %v", err)
	}
	if num.Phone != "13800000000" {
		t.Fatalf("phone = %q, want the first real number", num.Phone)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.releases) != 1 || f.releases[0] != "17000000001" {
		t.Fatalf("released = %v, want the virtual number handed straight back", f.releases)
	}
}

func TestAcquirePhoneFailsWhenEveryDrawIsVirtual(t *testing.T) {
	f := &fakeQoderSMS{phones: []string{"17100000001"}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := qoderSMSClient(t, srv, `"dup_retries":2`)

	if _, err := c.AcquirePhone(context.Background(), core.SMSOpts{}, "", nil); err == nil {
		t.Fatal("AcquirePhone accepted a virtual number")
	} else if !strings.Contains(err.Error(), "virtual") {
		t.Fatalf("error = %q, want it to name the virtual-number guard", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.releases) != 2 {
		t.Fatalf("released = %v, want every virtual draw handed back", f.releases)
	}
}

func TestAcquirePhoneKeepsPinnedAccountNumber(t *testing.T) {
	f := &fakeQoderSMS{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := qoderSMSClient(t, srv, "")

	num, err := c.AcquirePhone(context.Background(), core.SMSOpts{}, "17000000001", nil)
	if err != nil {
		t.Fatalf("AcquirePhone: %v", err)
	}
	if num.Phone != "17000000001" || !num.Reused {
		t.Fatalf("number = %+v, want the pinned account number", num)
	}
}

func TestReleaseAutoPhoneKeepsPinnedAccountNumber(t *testing.T) {
	f := &fakeQoderSMS{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := qoderSMSClient(t, srv, "")
	job := &autoJob{state: core.AutoLoginRunning}

	c.releaseAutoPhone(context.Background(), job, core.SMSOpts{}, core.SMSNumber{
		Phone:  "17000000001",
		Reused: true,
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.releases) != 0 {
		t.Fatalf("released = %v, want the account's pinned number left alone", f.releases)
	}
}

func TestReleaseAutoPhoneReturnsFreshNumber(t *testing.T) {
	f := &fakeQoderSMS{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := qoderSMSClient(t, srv, "")
	job := &autoJob{state: core.AutoLoginRunning}

	c.releaseAutoPhone(context.Background(), job, core.SMSOpts{}, core.SMSNumber{
		Phone: "13800000000",
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.releases) != 1 || f.releases[0] != "13800000000" {
		t.Fatalf("released = %v, want the fresh number handed back", f.releases)
	}
}
