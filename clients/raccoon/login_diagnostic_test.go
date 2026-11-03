package raccoon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// 手工登录的拒绝必须留下厂商原始诊断，并且落文件（issue #141）。
//
// 现场的形状：报告方要做「官方客户端手工登录一次」的对照，用来判断厂商是
// 「IP/设备信誉被风控」还是「自动化指纹被识别」。而这条路径上：
//
//  1. `smsSendError`（login.go）把整条 `apiError` 折叠成一句固定中文，
//     厂商的 Status / Code / IAMCode / RequestID **全部丢掉**——
//     request_id 是向厂商追问某一次拒绝的唯一把手；
//  2. 交互式登录全程没有任何 `deps.Log`/`logf`，所以事后 client2api.log 里
//     **连「被拒过」都看不到**（与 #141 方向 4 的环/文件分面同型，只是换了条路径）。
//
// 判据因此不是「文案里有滑块」——那条早就有测试——而是「机器可读的部分活着」。
// ---------------------------------------------------------------------------

// TestACaptchaRefusalKeepsTheVendorDiagnostics 钉住折叠时**附加**而不是替换。
func TestACaptchaRefusalKeepsTheVendorDiagnostics(t *testing.T) {
	captcha := &apiError{
		Status:    400,
		IAMCode:   "COMMON_INVALID_ARGUMENT",
		Message:   "captcha_token invalid",
		RequestID: "req-abc123",
	}
	got := smsSendError(captcha).Error()
	// 人话在前（操作员能照着做）。
	if !strings.Contains(got, "滑块") {
		t.Fatalf("captcha refusal = %q, want the operator-facing slider hint to survive", got)
	}
	// 机器码在后（方向 1 靠它定性）。
	for _, want := range []string{"HTTP 400", "COMMON_INVALID_ARGUMENT", "req-abc123"} {
		if !strings.Contains(got, want) {
			t.Errorf("captcha refusal = %q 丢了 %q：厂商原始诊断被折叠掉了，方向 1 无从判断"+
				"（request_id 还是向厂商追问的唯一把手）", got, want)
		}
	}

	// 数字码那条腿同样要保住。
	legacy := &apiError{Status: 400, Code: 100006, Message: "captcha_verify_error"}
	if got := smsSendError(legacy).Error(); !strings.Contains(got, "100006") {
		t.Errorf("legacy captcha refusal = %q, 丢了数字码 100006", got)
	}

	// 非 captcha 的拒绝照旧原样透传（不能被折叠，也不能被改写）。
	phone := &apiError{Status: 400, IAMCode: "COMMON_INVALID_ARGUMENT", Message: "phone number invalid"}
	if got := smsSendError(phone).Error(); strings.Contains(got, "滑块") {
		t.Errorf("phone refusal = %q, must not be reported as a captcha problem", got)
	}
}

// TestDiagnosticRendersOnlyWhatTheVendorGave: 空诊断不能吐出半截标点，
// 否则每条日志尾都挂一个空括号——读的人还得先分辨那是「没有」还是「没打印」。
func TestDiagnosticRendersOnlyWhatTheVendorGave(t *testing.T) {
	if got := (&apiError{Status: 200}).diagnostic(); got != "" {
		t.Errorf("a 200 with no codes must render no diagnostic, got %q", got)
	}
	if got := (&apiError{Code: 100006}).diagnostic(); got != "code 100006" {
		t.Errorf("numeric code alone = %q, want %q", got, "code 100006")
	}
	if got := (&apiError{Status: 429, IAMCode: "RATE_LIMITED", RequestID: "r-1"}).diagnostic(); got != "HTTP 429 · RATE_LIMITED · request_id r-1" {
		t.Errorf("full diagnostic = %q", got)
	}
	// 符号码存在时不该再报数字码 0（IAM 只有前者）。
	if got := (&apiError{Status: 400, IAMCode: "COMMON_INVALID_ARGUMENT"}).diagnostic(); strings.Contains(got, "code 0") {
		t.Errorf("empty numeric code leaked into the diagnostic: %q", got)
	}
	// 非 apiError（传输失败）没有厂商答复可引，不能编一个。
	if got := apiDiagnostic(fmt.Errorf("read tcp: connection reset")); got != "" {
		t.Errorf("a transport failure must have no diagnostic, got %q", got)
	}
}

// TestTheManualLoginRefusalReachesTheLog is the FUNCTIONAL check: drive the real
// handler and assert the line lands on the sink.  A static assertion can only
// prove "logLoginFailure exists"; only running it proves the handler calls it --
// and a missing call is exactly the shape that left this path dark.
func TestTheManualLoginRefusalReachesTheLog(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	// A vendor that refuses the send with a captcha verdict plus its own code.
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"COMMON_INVALID_ARGUMENT","message":"captcha_token invalid","request_id":"req-live-1"}`))
	}))
	defer vendor.Close()

	cl, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(`{"base_url":` + strconv.Quote(vendor.URL) + `}`),
		HTTPClient: vendor.Client(),
		Logf:       logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := cl.(*Client)

	sess := &loginSession{now: c.now, sessionID: "raccoon:test"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, pathSmsSend,
		strings.NewReader(`{"phone":"13800000000","captchaParam":"tracer"}`))
	c.handleSmsSend(sess)(rec, req)

	// 页面侧照旧拿到那句人话（行为不变）。
	var page map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode the page response: %v (%s)", err, rec.Body.String())
	}
	if msg, _ := page["message"].(string); !strings.Contains(msg, "滑块") {
		t.Fatalf("page message = %q, want the slider hint to survive", msg)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(lines) == 0 {
		t.Fatal("手工登录被拒后一行日志都没有：事后 client2api.log 里连「被拒过」都看不到（issue #141）")
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"manual login", "send", "req-live-1", "COMMON_INVALID_ARGUMENT"} {
		if !strings.Contains(joined, want) {
			t.Errorf("落盘的日志里找不到 %q：\n%s", want, joined)
		}
	}
	// 手机号必须打码，不能原样进日志。
	if strings.Contains(joined, "13800000000") {
		t.Errorf("日志里出现了完整手机号：\n%s", joined)
	}
}
