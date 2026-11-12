package loomy

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// #259：refreshModelsAsync 把「没有账号」（ErrNotConfigured，状态）当失败无条件
// logf——pool 空时 30s 限流成为恒定发射器，3 天 1477 条「client is not
// configured」刷满日志检索面。修法 = 与同步 RefreshModels 的透传语义一致：
// NotConfigured 静默返回（不 logf、不 noteFailure），真上游失败照常日志。
// ---------------------------------------------------------------------------

// loggedLines installs a Logf capture and returns the reader.
func loggedLines() (func(string, ...any), func() string) {
	var sb strings.Builder
	done := make(chan struct{}, 64)
	done <- struct{}{}
	logf := func(format string, args ...any) {
		<-done
		sb.WriteString(format + "\n")
		done <- struct{}{}
	}
	read := func() string {
		<-done
		s := sb.String()
		done <- struct{}{}
		return s
	}
	return logf, read
}

func noAccountConfigJSON() string {
	return `{"credentials": [], "models": {"ttl_minutes": 1}}`
}

// TestAsyncModelRefreshStaysSilentWhenNotConfigured 钉住静默：pool 空时后台刷新
// 不写任何日志行（30s 限流不再是恒定发射器）。
func TestAsyncModelRefreshStaysSilentWhenNotConfigured(t *testing.T) {
	logf, read := loggedLines()
	c := newTestClientInDir(t, t.TempDir(), noAccountConfigJSON(), nil)
	c.deps.Logf = logf

	c.refreshModelsAsync()

	// fetchModels 在 candidates==0 时同步返回 ErrNotConfigured，goroutine 很快
	// 结束；等 loading 标志落下再核对。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.modelsMu.Lock()
		loading := c.modelsLoading
		c.modelsMu.Unlock()
		if !loading {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	if lines := read(); strings.Contains(lines, "client is not configured") {
		t.Fatalf("NotConfigured 被写进了日志：%q（issue #259：状态不是失败）", lines)
	}
}

// TestAsyncModelRefreshLogsRealFailures 钉住反向钉：真上游失败（传输层拒绝）的
// 日志行为不回退——有一个账号、但上游不可达。
func TestAsyncModelRefreshLogsRealFailures(t *testing.T) {
	logf, read := loggedLines()
	// 拒绝一切连接的 RoundTripper：每次调用都返回一个真传输错误。
	alwaysErr := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errRefusedForTest
	})
	c := newTestClientInDir(t, t.TempDir(), noAccountConfigJSON(), alwaysErr)
	c.deps.Logf = logf
	seedAccount(t, c, "acc-live", "tok", time.Now().Add(time.Hour).UnixMilli())

	c.refreshModelsAsync()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.modelsMu.Lock()
		loading := c.modelsLoading
		c.modelsMu.Unlock()
		if !loading {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lines := read(); !strings.Contains(lines, "loomy: model list:") {
		t.Fatalf("真上游失败没有留下任何日志行（反向钉失守）：%q", lines)
	}
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// errRefusedForTest is a transport-layer error the always-error round tripper
// hands back, so the async refresh sees a real upstream failure.
var errRefusedForTest = &testNetError{op: "dial", msg: "connection refused for test"}

type testNetError struct {
	op  string
	msg string
}

func (e *testNetError) Error() string   { return e.op + ": " + e.msg }
func (e *testNetError) Timeout() bool   { return false }
func (e *testNetError) Temporary() bool { return false }
