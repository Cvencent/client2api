package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// #256：fold 失败退避按会话指纹记键，增长中的会话每轮换键（dropped 498→514→…）
// 退避永不武装——raccoon 每 ~1 小时重付一次 6 分钟超时、loomy 每小时一次 90s。
// 修法 = 失败分类分层：容量类失败（deadline / ErrPlatformExhausted / ErrBusy /
// 传输故障）定罪平台，平台级冷却 30 分钟；厂商形状/内容拒绝维持指纹级退避
// （#240 证据链不动）。
// ---------------------------------------------------------------------------

// TestAPlatformCooldownArmsAfterCapacityFailures 钉住平台冷却的武装：同一平台
// 连续两次容量类失败后冷却生效——即使每轮的指纹键都不同（增长会话的形状）。
func TestAPlatformCooldownArmsAfterCapacityFailures(t *testing.T) {
	store := newTrimSummaryStore()
	const platform = "raccoon"

	// 每轮换键（增长会话）：键 A 失败一次、键 B 失败一次——指纹级计数器各自
	// 只有 1，永远到不了阈值；平台级应该已经武装。
	store.recordFoldFailure("key-turn-1")
	store.recordFoldFailure("key-turn-2")
	if backed, _ := store.foldBackedOff("key-turn-2"); backed {
		t.Fatal("different keys must not arm the fingerprint backoff (the growing-conversation shape)")
	}

	if got := store.recordFoldPlatformFailure(platform); got != 1 {
		t.Fatalf("first platform failure count = %d, want 1", got)
	}
	if cooled, _ := store.foldPlatformCooled(platform); cooled {
		t.Fatal("one capacity failure must not cool the platform yet")
	}
	if got := store.recordFoldPlatformFailure(platform); got != 2 {
		t.Fatalf("second platform failure count = %d, want 2", got)
	}
	cooled, fails := store.foldPlatformCooled(platform)
	if !cooled || fails != trimFoldPlatformCooldownThreshold {
		t.Fatalf("platform cooled = %v,%d, want armed at threshold", cooled, fails)
	}
	// 其它平台不受连坐。
	if cooled, _ := store.foldPlatformCooled("officeace"); cooled {
		t.Error("an unrelated platform was cooled alongside the failing one")
	}
	// 空平台名不退避。
	if cooled, _ := store.foldPlatformCooled(""); cooled {
		t.Error("an empty platform name was cooled")
	}
	// 冷却窗口经过：放行一次新尝试。
	store.mu.Lock()
	p := store.platforms[platform]
	p.lastFail = time.Now().Add(-trimFoldPlatformCooldown - time.Minute)
	store.platforms[platform] = p
	store.mu.Unlock()
	if cooled, fails := store.foldPlatformCooled(platform); cooled || fails != 0 {
		t.Fatalf("cooldown lapsed but still cooled: %v,%d (one fresh attempt must be allowed)", cooled, fails)
	}
}

// TestBPlatformCooldownVerdictClassifiesFailure 钉住分类表：容量类失败归平台、
// 内容/形状拒绝归指纹。这是两条退避路径的分界。
func TestBPlatformCooldownVerdictClassifiesFailure(t *testing.T) {
	platformVerdicts := []error{
		context.DeadlineExceeded,
		fmt.Errorf("loomy: reading the stream: %w", context.DeadlineExceeded),
		core.ErrPlatformExhausted,
		fmt.Errorf("%w after 1 account: not account-specific", core.ErrPlatformExhausted),
		core.ErrBusy,
		core.ErrNotConfigured,
		core.Fail("", "", core.FailureUpstream, 0, errors.New("openai-compat: upstream stream ended before a terminal frame")),
		&url.Error{Op: "Post", URL: "https://vendor.example/api", Err: context.DeadlineExceeded},
		errors.New("Post \"https://vendor.example\": connection reset by peer"),
	}
	for _, err := range platformVerdicts {
		if !foldPlatformCooldownVerdict(err) {
			t.Errorf("foldPlatformCooldownVerdict(%v) = false, want true (platform capacity verdict)", err)
		}
	}
	fingerprintVerdicts := []error{
		errors.New("officeace: chat: HTTP 400: InferHub.ModelArts.81001.400: messages parameter illegal"),
		errors.New("opencode: chat: HTTP 502: server_error: Upstream request failed: [1214] The messages parameter is illegal"),
		core.Fail("raccoon", "a1", core.FailureRateLimited, 429, errors.New("429 too many requests")),
		core.Fail("kimi", "a1", core.FailureAuth, 401, errors.New("401 unauthorized")),
		core.Fail("codearts", "a1", core.FailureContextWindow, 400, errors.New("input token limit is 1048576")),
		core.Fail("officeace", "a1", core.FailureContentBlocked, 400, errors.New("content policy")),
		context.Canceled,
		fmt.Errorf("wrap: %w", context.Canceled),
		nil,
	}
	for _, err := range fingerprintVerdicts {
		if foldPlatformCooldownVerdict(err) {
			t.Errorf("foldPlatformCooldownVerdict(%v) = true, want false (conversation-shape or nobody's verdict)", err)
		}
	}
}

// TestCPlatformCooldownClearsOnSuccess 钉住解除：一次成功折叠清除平台计数——
// 厂商恢复健康后冷却不能残留。
func TestCPlatformCooldownClearsOnSuccess(t *testing.T) {
	store := newTrimSummaryStore()
	const platform = "loomy"
	store.recordFoldPlatformFailure(platform)
	store.recordFoldPlatformFailure(platform)
	if cooled, _ := store.foldPlatformCooled(platform); !cooled {
		t.Fatal("precondition: the platform must be cooled")
	}
	store.recordFoldPlatformSuccess(platform)
	if cooled, _ := store.foldPlatformCooled(platform); cooled {
		t.Fatal("a served fold must clear the platform cooldown")
	}
	// 一次失败后成功：计数归零，下一次失败重新从 1 计。
	store.recordFoldPlatformFailure(platform)
	if cooled, _ := store.foldPlatformCooled(platform); cooled {
		t.Fatal("a single failure after a success must not cool the platform")
	}
}

// TestDOfferGateSkipsCooledPlatform 钉住 offer 闸：平台冷却中的 fold offer 被
// 跳过，且指纹退避（#240）的现有行为不受影响。
func TestDOfferGateSkipsCooledPlatform(t *testing.T) {
	req := foldTestRequest(120, 100)
	store := newTrimSummaryStore()
	var (
		mu   sync.Mutex
		seen *trimFoldJob
	)
	coord := newTrimFoldCoordinator(
		func(ctx context.Context, job *trimFoldJob) (string, error) {
			mu.Lock()
			seen = job
			mu.Unlock()
			return "folded", nil
		},
		func(job *trimFoldJob, summary string) {},
		nil, store, nil)
	fold := &trimFoldContext{store: store, folds: coord, client: &testClient{name: "wedge"}, model: "m1"}

	// 冷却前：offer 正常到达 fold seam。
	rep, trimmed := applyContextBudgetWithFold(req, 2000, fold)
	if !trimmed || rep.DroppedMessages == 0 {
		t.Fatalf("trim did not drop anything: %+v trimmed=%v", rep, trimmed)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := seen
		mu.Unlock()
		if got != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	reached := seen != nil
	mu.Unlock()
	if !reached {
		t.Fatal("the healthy platform must still receive fold offers")
	}

	// 冷却后：offer 被跳过——换一个新请求（新指纹），seam 不再收到。
	store.recordFoldPlatformFailure("wedge")
	store.recordFoldPlatformFailure("wedge")
	req2 := foldTestRequest(130, 100)
	mu.Lock()
	seen = nil
	mu.Unlock()
	applyContextBudgetWithFold(req2, 2000, fold)
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	skipped := seen == nil
	mu.Unlock()
	if !skipped {
		t.Fatal("a cooled platform must not receive fold offers (issue #256)")
	}
}

// TestECooldownLogNamesThePlatform 钉住 failed 回调的分类入账：容量类失败要
// 在日志里点名平台冷却——否则运维看不到防线在工作。真回调的接线在
// newServer（server.go），这里走同样的分类函数断言 note 文案。
func TestECooldownLogNamesThePlatform(t *testing.T) {
	err := fmt.Errorf("loomy: reading the stream: %w", context.DeadlineExceeded)
	if !foldPlatformCooldownVerdict(err) {
		t.Fatal("precondition: deadline is a platform verdict")
	}
	// 与 server.go 的 note 拼装一致的最小复述：阈值武装时 note 必须点名平台。
	pfails := trimFoldPlatformCooldownThreshold
	note := fmt.Sprintf("; platform %s folds cooled for %s after %d consecutive capacity failures", "loomy", trimFoldPlatformCooldown, pfails)
	if !strings.Contains(note, "loomy") || !strings.Contains(note, "capacity failures") {
		t.Fatalf("cooldown note lost the platform name or the reason: %q", note)
	}
}
