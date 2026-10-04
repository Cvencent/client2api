package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// renewFakeClient is a minimal AccountManager: it lists a fixed set of records
// and records every RefreshAccount call so a test can tell exactly which
// accounts the sweep decided to renew.
type renewFakeClient struct {
	name     string
	accounts []core.AccountRecord

	mu      sync.Mutex
	calls   []string
	err     error
	results []core.RefreshResult
}

func (f *renewFakeClient) Name() string { return f.name }
func (f *renewFakeClient) Models(context.Context) ([]core.Model, error) {
	return nil, nil
}
func (f *renewFakeClient) Chat(context.Context, *core.ChatRequest) (core.Stream, error) {
	return nil, core.ErrUnsupported
}
func (f *renewFakeClient) Status(context.Context) core.Status {
	return core.Status{Name: f.name}
}

func (f *renewFakeClient) AccountFields(context.Context) []core.FieldSpec { return nil }
func (f *renewFakeClient) Accounts(context.Context) ([]core.AccountRecord, error) {
	return f.accounts, nil
}
func (f *renewFakeClient) AddAccount(context.Context, core.AccountSpec) (core.AccountRecord, error) {
	return core.AccountRecord{}, core.ErrUnsupported
}
func (f *renewFakeClient) RemoveAccount(context.Context, string) error { return core.ErrUnsupported }
func (f *renewFakeClient) SetAccountEnabled(context.Context, string, bool) error {
	return core.ErrUnsupported
}
func (f *renewFakeClient) TestAccount(context.Context, string) (core.TestResult, error) {
	return core.TestResult{}, core.ErrUnsupported
}
func (f *renewFakeClient) RefreshAccount(_ context.Context, id string) ([]core.RefreshResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	if f.err != nil {
		return nil, f.err
	}
	if f.results != nil {
		return f.results, nil
	}
	return []core.RefreshResult{{AccountID: id, OK: true}}, nil
}

func (f *renewFakeClient) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func renewClient(name string, accounts ...core.AccountRecord) (*core.Registry, *renewFakeClient) {
	f := &renewFakeClient{name: name, accounts: accounts}
	reg := core.NewRegistry()
	reg.Add(f)
	return reg, f
}

// TestRenewSweepPicksOnlyAccountsInsideTheirMargin pins the selection rule:
// an enabled, refreshable account whose announced expiry is inside its window
// is renewed, and everything else is left alone.
func TestRenewSweepPicksOnlyAccountsInsideTheirMargin(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	rec := func(id, exp string, enabled bool, refreshable any) core.AccountRecord {
		fields := map[string]any{}
		if refreshable != nil {
			fields["refreshable"] = refreshable
		}
		return core.AccountRecord{ID: id, ExpiresAt: exp, Enabled: enabled, State: "ready", Fields: fields}
	}

	reg, f := renewClient("codearts",
		rec("soon", at(10*time.Minute), true, true),
		rec("far", at(10*time.Hour), true, true),
		rec("off", at(10*time.Minute), false, true),
		rec("norenew", at(10*time.Minute), true, false),
		rec("noexpiry", "", true, true),
		rec("garbage", "tomorrow", true, true),
		rec("unix", strconv.FormatInt(now.Add(5*time.Minute).Unix(), 10), true, true),
	)

	refreshExpiringAccounts(context.Background(), reg, now, quietLogger())

	got := f.called()
	want := []string{"soon", "unix"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("renewed %v, want %v", got, want)
	}
}

// TestRenewSweepHonoursTheModuleMargin proves the sweep reads the margin the
// module publishes instead of always using the default: the same expiry is
// renewed by an account that asks for a wide window and skipped by one that
// falls back to the 30-minute default.
func TestRenewSweepHonoursTheModuleMargin(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	exp := now.Add(45 * time.Minute).UTC().Format(time.RFC3339)

	reg, f := renewClient("codearts",
		core.AccountRecord{ID: "wide", Enabled: true, ExpiresAt: exp,
			Fields: map[string]any{"refreshable": true, "refresh_margin_seconds": int64(3600)}},
		core.AccountRecord{ID: "default", Enabled: true, ExpiresAt: exp,
			Fields: map[string]any{"refreshable": true}},
	)

	refreshExpiringAccounts(context.Background(), reg, now, quietLogger())

	if got := f.called(); len(got) != 1 || got[0] != "wide" {
		t.Fatalf("renewed %v, want [wide]", got)
	}
}

// TestRenewSweepContinuesPastFailures is the resilience half: a RefreshAccount
// that errors (or reports a per-account failure) must be logged and must not
// stop the accounts after it.
func TestRenewSweepContinuesPastFailures(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	exp := now.Add(5 * time.Minute).UTC().Format(time.RFC3339)
	acct := func(id string) core.AccountRecord {
		return core.AccountRecord{ID: id, Enabled: true, ExpiresAt: exp,
			Fields: map[string]any{"refreshable": true}}
	}

	// The first client's RefreshAccount itself fails.
	reg, f := renewClient("broken", acct("a1"), acct("a2"))
	f.err = errors.New("upstream refused")
	var buf bytes.Buffer
	refreshExpiringAccounts(context.Background(), reg, now, log.New(&buf, "", 0))
	if got := f.called(); len(got) != 2 {
		t.Fatalf("a failing refresh stopped the sweep early: called %v", got)
	}
	if !strings.Contains(buf.String(), "upstream refused") {
		t.Errorf("failure was not logged: %q", buf.String())
	}

	// A per-account failure inside an otherwise successful result is logged
	// too, and other clients still run.
	reg2, f2 := renewClient("partial", acct("b1"))
	f2.results = []core.RefreshResult{{AccountID: "b1", Error: "no refresh token is stored"}}
	other := &renewFakeClient{name: "other",
		accounts: []core.AccountRecord{acct("c1")}}
	reg2.Add(other)
	buf.Reset()
	refreshExpiringAccounts(context.Background(), reg2, now, log.New(&buf, "", 0))
	if !strings.Contains(buf.String(), "no refresh token is stored") {
		t.Errorf("per-account failure was not logged: %q", buf.String())
	}
	if got := other.called(); len(got) != 1 {
		t.Errorf("a failure on one client skipped another: %v", got)
	}
}

// TestParseAccountExpiryAcceptsPublishedShapes covers the formats modules
// actually hand to the panel.
func TestParseAccountExpiryAcceptsPublishedShapes(t *testing.T) {
	want := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		raw  string
		want time.Time
		ok   bool
	}{
		{want.Format(time.RFC3339), want, true},
		{strconv.FormatInt(want.Unix(), 10), want, true},
		{strconv.FormatInt(want.UnixMilli(), 10), want, true},
		{"", time.Time{}, false},
		{"0", time.Time{}, false},
		{"-5", time.Time{}, false},
		{"tomorrow", time.Time{}, false},
	}
	for _, tc := range cases {
		got, ok := parseAccountExpiry(tc.raw)
		if ok != tc.ok {
			t.Errorf("parseAccountExpiry(%q) ok = %v, want %v", tc.raw, ok, tc.ok)
			continue
		}
		if ok && !got.Equal(tc.want) {
			t.Errorf("parseAccountExpiry(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
