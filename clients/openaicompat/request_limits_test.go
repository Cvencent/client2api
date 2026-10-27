package openaicompat

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"client2api/internal/core"
)

func newTestRequestLimiter(t *testing.T, path string, limits []core.SourceRequestLimit) *sourceRequestLimiter {
	t.Helper()
	l, err := newSourceRequestLimiter(path, limits)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func acquireRequestLimitOK(t *testing.T, l *sourceRequestLimiter, account string, now time.Time) {
	t.Helper()
	block, err := l.Acquire(account, now)
	if err != nil {
		t.Fatal(err)
	}
	if block != nil {
		t.Fatalf("account %q was blocked unexpectedly: %+v", account, block)
	}
}

func acquireRequestLimitBlocked(t *testing.T, l *sourceRequestLimiter, account string, now time.Time, scope string) *requestLimitBlock {
	t.Helper()
	block, err := l.Acquire(account, now)
	if err != nil {
		t.Fatal(err)
	}
	if block == nil {
		t.Fatalf("account %q was allowed, want %s block", account, scope)
	}
	if block.Scope != scope {
		t.Fatalf("block scope = %q, want %q", block.Scope, scope)
	}
	return block
}

func TestRequestLimiterRollingPerAccount(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	l := newTestRequestLimiter(t, filepath.Join(t.TempDir(), "request_limits.json"), []core.SourceRequestLimit{{
		Scope: "account", Limit: 100, Window: "24h", Reset: "rolling",
	}})
	for i := 0; i < 100; i++ {
		acquireRequestLimitOK(t, l, "key-a", now.Add(time.Duration(i)*time.Second))
	}
	block := acquireRequestLimitBlocked(t, l, "key-a", now.Add(100*time.Second), "account")
	if want := now.Add(24 * time.Hour); !block.Until.Equal(want) {
		t.Fatalf("block until = %s, want %s", block.Until, want)
	}
	acquireRequestLimitOK(t, l, "key-b", now.Add(100*time.Second))
}

func TestRequestLimiterCalendarResetsAtConfiguredTime(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 27, 7, 59, 59, 0, loc)
	l := newTestRequestLimiter(t, filepath.Join(t.TempDir(), "request_limits.json"), []core.SourceRequestLimit{{
		Scope: "account", Limit: 1, Window: "24h", Reset: "calendar", Timezone: "Asia/Shanghai", ResetAt: "08:00",
	}})
	acquireRequestLimitOK(t, l, "key-a", now)
	acquireRequestLimitBlocked(t, l, "key-a", now, "account")
	acquireRequestLimitOK(t, l, "key-a", time.Date(2026, 10, 27, 8, 0, 0, 0, loc))
}

func TestRequestLimiterFirstWindow(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	l := newTestRequestLimiter(t, filepath.Join(t.TempDir(), "request_limits.json"), []core.SourceRequestLimit{{
		Scope: "account", Limit: 1, Window: "1m", Reset: "first",
	}})
	acquireRequestLimitOK(t, l, "key-a", now)
	acquireRequestLimitBlocked(t, l, "key-a", now.Add(59*time.Second), "account")
	acquireRequestLimitOK(t, l, "key-a", now.Add(time.Minute))
}

func TestRequestLimiterMultiRuleChargesAtomically(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	l := newTestRequestLimiter(t, filepath.Join(t.TempDir(), "request_limits.json"), []core.SourceRequestLimit{
		{Scope: "account", Limit: 2, Window: "1m", Reset: "first"},
		{Scope: "account", Limit: 3, Window: "24h", Reset: "rolling"},
	})
	acquireRequestLimitOK(t, l, "key-a", now)
	acquireRequestLimitOK(t, l, "key-a", now.Add(10*time.Second))
	acquireRequestLimitBlocked(t, l, "key-a", now.Add(20*time.Second), "account")
	acquireRequestLimitOK(t, l, "key-a", now.Add(61*time.Second))
	acquireRequestLimitBlocked(t, l, "key-a", now.Add(62*time.Second), "account")
}

func TestRequestLimiterSourceScopeBlocksAllAccounts(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	l := newTestRequestLimiter(t, filepath.Join(t.TempDir(), "request_limits.json"), []core.SourceRequestLimit{{
		Scope: "source", Limit: 1, Window: "24h", Reset: "rolling",
	}})
	acquireRequestLimitOK(t, l, "key-a", now)
	acquireRequestLimitBlocked(t, l, "key-b", now.Add(time.Second), "source")
	if block := l.AccountBlocked("key-b", now.Add(time.Second)); block != nil {
		t.Fatalf("source rule appeared as an account block: %+v", block)
	}
	if block := l.SourceBlocked(now.Add(time.Second)); block == nil || block.Scope != "source" {
		t.Fatalf("SourceBlocked = %+v, want source block", block)
	}
}

func TestRequestLimiterPersistsAcrossReload(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "request_limits.json")
	limits := []core.SourceRequestLimit{{Scope: "account", Limit: 1, Window: "24h", Reset: "rolling"}}
	first := newTestRequestLimiter(t, path, limits)
	acquireRequestLimitOK(t, first, "key-a", now)

	second := newTestRequestLimiter(t, path, limits)
	acquireRequestLimitBlocked(t, second, "key-a", now.Add(time.Second), "account")
}

func TestRequestLimiterWriteFailureFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "request_limits.json")
	l := newTestRequestLimiter(t, path, []core.SourceRequestLimit{{
		Scope: "account", Limit: 1, Window: "24h", Reset: "rolling",
	}})
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire("key-a", now); err == nil {
		t.Fatal("Acquire succeeded although the counter file could not be written")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	acquireRequestLimitOK(t, l, "key-a", now.Add(time.Second))
}

func TestRequestLimiterCorruptFileRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request_limits.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newSourceRequestLimiter(path, nil); err == nil {
		t.Fatal("corrupt counter file was accepted")
	}
}

func TestRequestLimiterRejectsSubSecondWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request_limits.json")
	_, err := newSourceRequestLimiter(path, []core.SourceRequestLimit{{
		Scope: "account", Limit: 1, Window: "500ms",
	}})
	if err == nil {
		t.Fatal("sub-second window was accepted; the panel cannot represent it")
	}
}

func TestRequestLimiterRejectsInvalidCalendarWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request_limits.json")
	_, err := newSourceRequestLimiter(path, []core.SourceRequestLimit{{
		Scope: "account", Limit: 1, Window: "7h", Reset: "calendar",
	}})
	if err == nil {
		t.Fatal("calendar window that does not divide 24h was accepted")
	}
}

func TestRequestLimiterLimitChangeKeepsUsage(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	l := newTestRequestLimiter(t, filepath.Join(t.TempDir(), "request_limits.json"), []core.SourceRequestLimit{{
		Scope: "account", Limit: 1, Window: "24h", Reset: "rolling",
	}})
	acquireRequestLimitOK(t, l, "key-a", now)
	if err := l.Configure([]core.SourceRequestLimit{{Scope: "account", Limit: 2, Window: "24h", Reset: "rolling"}}); err != nil {
		t.Fatal(err)
	}
	acquireRequestLimitOK(t, l, "key-a", now.Add(time.Second))
	acquireRequestLimitBlocked(t, l, "key-a", now.Add(2*time.Second), "account")
}
