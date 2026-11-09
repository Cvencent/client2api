package openaicompat

import (
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// #243：图片内容过滤是平台口径差异（officeace 拒 loomy 收）——归类为
// FailureImageRefused，nextPlatform 一次改道由决策表驱动。
// ---------------------------------------------------------------------------

func TestAImageRefusalMapsToImageRefusedKind(t *testing.T) {
	if got := classifyFailure(400, `image_url.url field is unsafe`); got != kindImageRefused {
		t.Fatalf("classifyFailure = %q, want kindImageRefused (issue #243)", got)
	}
	if got := classifyFailure(400, "the image is unsafe to serve"); got != kindImageRefused {
		t.Fatalf("classifyFailure = %q, want kindImageRefused (issue #243)", got)
	}
	// 反向钉：普通文本 400 不许误入图片类。
	if got := classifyFailure(400, `messages 参数非法`); got == kindImageRefused {
		t.Error("a plain 400 was classified as an image refusal (issue #243 反向钉)")
	}
	if got := classifyFailure(400, "invalid schema for tool"); got == kindImageRefused {
		t.Error("a schema 400 was classified as an image refusal (issue #243 反向钉)")
	}
}

func TestBImageRefusedNextPlatformInDecisionTable(t *testing.T) {
	if !core.NextPlatformOn(core.FailureImageRefused) {
		t.Error("FailureImageRefused must move platforms: the same image another filter may serve (issue #243)")
	}
	if core.Retryable(core.FailureImageRefused) {
		t.Error("FailureImageRefused must not rotate accounts on the SAME platform: every account there answers identically (issue #243)")
	}
	if core.FailureImageRefused == "" {
		t.Fatal("kind must be non-empty")
	}
}
