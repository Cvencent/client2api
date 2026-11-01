package trae

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// autologin_test.go covers the one-click flow's fast-fail paths and the
// page-driving helpers without a browser.  The full end-to-end run against a
// live console page needs the real DOM; the session-level exchange it ends
// in is already covered by TestWebLoginHappyPath.

// fakeTraeDriver records what the browser step was asked to do.
type fakeTraeDriver struct {
	mu       sync.Mutex
	url      string
	phone    string
	codeSeen string
	driveErr error
}

func (d *fakeTraeDriver) Drive(ctx context.Context, url, phone string, waitCode func(context.Context) (string, error), logf func(string, ...any)) error {
	d.mu.Lock()
	d.url = url
	d.phone = phone
	d.mu.Unlock()
	logf("浏览器步骤开始")
	if d.driveErr != nil {
		return d.driveErr
	}
	code, err := waitCode(ctx)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.codeSeen = code
	d.mu.Unlock()
	return nil
}

func shrinkTraeTimings(t *testing.T) {
	t.Helper()
	settle, sendWait := traePhoneSettle, traeSendCodeWait
	traePhoneSettle, traeSendCodeWait = 2*time.Millisecond, 30*time.Millisecond
	t.Cleanup(func() { traePhoneSettle, traeSendCodeWait = settle, sendWait })
}

func waitTraeJob(t *testing.T, c *Client, id string) core.AutoLoginJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		job, err := c.PollAutoLogin(context.Background(), id)
		if err != nil {
			t.Fatalf("PollAutoLogin: %v", err)
		}
		if job.State != core.AutoLoginRunning {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job never finished: %+v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTraeAutoLoginNeedsAnSMSToken(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	c := panelClient(t, nil, &loginTransport{})
	_, err := c.StartAutoLogin(context.Background(), core.AutoLoginRequest{})
	if err == nil {
		t.Fatal("StartAutoLogin without an SMS token should fail fast")
	}
	if !strings.Contains(err.Error(), "no SMS platform token") {
		t.Fatalf("err = %v, want the missing-token diagnosis", err)
	}
}

func TestTraeCancelUnknownJobIsNotAnError(t *testing.T) {
	c := panelClient(t, nil, &loginTransport{})
	if err := c.CancelAutoLogin(context.Background(), "no-such-job"); err != nil {
		t.Fatalf("CancelAutoLogin on an unknown id: %v", err)
	}
	if _, err := c.PollAutoLogin(context.Background(), "no-such-job"); err == nil {
		t.Fatal("PollAutoLogin on an unknown id should fail")
	}
}

// --- the page-driving helpers, tested against a fake page -------------------

type fakeTraePage struct {
	mu        sync.Mutex
	exists    map[string]bool
	fillCalls map[string]string
	clicks    []string
	href      string
}

func newFakeTraePage() *fakeTraePage {
	return &fakeTraePage{exists: map[string]bool{}, fillCalls: map[string]string{}}
}

func (p *fakeTraePage) Exists(_ context.Context, selector string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ok, seen := p.exists[selector]; seen {
		return ok, nil
	}
	// The phone input candidates exist; the send-code candidates do not, so
	// the text-based fallback path is exercised.
	return strings.Contains(selector, "tel") || strings.Contains(selector, "手机号"), nil
}

func (p *fakeTraePage) Click(_ context.Context, selector string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clicks = append(p.clicks, selector)
	return true, nil
}

func (p *fakeTraePage) Fill(_ context.Context, selector, value string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fillCalls[selector] = value
	return true, nil
}

func (p *fakeTraePage) EvalBool(_ context.Context, _ string) (bool, error) { return true, nil }

func (p *fakeTraePage) EvalString(_ context.Context, expression string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if expression == "location.href" {
		return p.href, nil
	}
	return "", nil
}

func TestTraeFillFirstPicksTheFirstExistingSelector(t *testing.T) {
	page := newFakeTraePage()
	var logs []string
	ok := fillTraeFirst(context.Background(), page, traeSelPhone, "13800000000", func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}, "手机号输入框")
	if !ok {
		t.Fatal("fillTraeFirst reported no match though input[type=tel] exists")
	}
	found := false
	for sel, v := range page.fillCalls {
		if sel == traeSelPhone[0] && v == "13800000000" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fillCalls = %v, want the first candidate filled", page.fillCalls)
	}
	if !strings.Contains(strings.Join(logs, "\n"), traeSelPhone[0]) {
		t.Fatalf("logs = %v, want the matched selector named", logs)
	}
}

func TestTraeFillFirstReportsNoMatch(t *testing.T) {
	page := newFakeTraePage()
	// Pre-seed every candidate as absent so Exists answers immediately, and
	// shrink the probe window so the sweep over six candidates stays fast.
	for _, sel := range traeSelPhone {
		page.exists[sel] = false
	}
	restore := traeProbeWait
	traeProbeWait = 5 * time.Millisecond
	t.Cleanup(func() { traeProbeWait = restore })
	ok := fillTraeFirst(context.Background(), page, traeSelPhone, "13800000000", func(string, ...any) {}, "手机号输入框")
	if ok {
		t.Fatal("fillTraeFirst reported a match though every candidate is absent")
	}
}

func TestTraeWaitRedirectReturnsOnTheCallback(t *testing.T) {
	page := newFakeTraePage()
	page.href = "http://127.0.0.1:18080/authorize?refreshToken=abc&userInfo=%7B%7D"
	if err := waitTraeRedirect(context.Background(), page, func(string, ...any) {}); err != nil {
		t.Fatalf("waitTraeRedirect: %v", err)
	}
}

func TestTraeWaitRedirectReportsAMissingRedirect(t *testing.T) {
	restore := traeRedirectWait
	traeRedirectWait = 60 * time.Millisecond
	t.Cleanup(func() { traeRedirectWait = restore })

	page := newFakeTraePage()
	page.href = "https://www.trae.cn/authorization?..."
	err := waitTraeRedirect(context.Background(), page, func(string, ...any) {})
	if err == nil {
		t.Fatal("the wait reported success though the page never reached the callback")
	}
	if !strings.Contains(err.Error(), "页面没有跳转到登录回调") {
		t.Fatalf("err = %v, want the missing-redirect diagnosis", err)
	}
}
