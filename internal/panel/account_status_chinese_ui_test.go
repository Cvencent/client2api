package panel

import (
	"strings"
	"testing"
)

// TestAccountStatusNotesRenderInChinese pins the account-pool status column to
// Chinese display text. The modules still use English internally for stable
// machine-readable state, so the panel is the translation boundary.
func TestAccountStatusNotesRenderInChinese(t *testing.T) {
	src := string(indexHTML)
	body := poolStatsFuncBody(t, src, "statusNoteText")
	for _, want := range []string{
		`const NOTE_WORDS = {`,
		`const NOTE_SENTENCES = {`,
		`"no credential has been imported yet": "尚未导入凭据"`,
		`"not configured": "尚未配置"`,
		`"disabled by operator": "已被操作员停用"`,
		`[/^plan JWT in the ZCode desktop client credential store$/i, "ZCode 桌面客户端凭据库中的套餐 JWT"]`,
		`const cjk = /[\u3400-\u9fff]/;`,
		`return "状态异常（悬停查看原始说明）";`,
	} {
		if !strings.Contains(src, want) && !strings.Contains(body, want) {
			t.Errorf("账号状态说明缺少中文翻译规则：%s", want)
		}
	}

	cell := poolStatsFuncBody(t, src, "noteCell")
	if !strings.Contains(cell, "statusNoteText(n, a)") {
		t.Error("noteCell 没有经过 statusNoteText，英文状态说明仍可能原样显示")
	}
	if strings.Contains(cell, "esc(zh || n)") {
		t.Error("noteCell 仍会直接渲染未翻译的原始状态说明")
	}
}

// TestAccountStatusNoteTranslationCoversKnownModulePhrases keeps the panel-side
// translation table aligned with fixed notes that modules currently emit.
func TestAccountStatusNoteTranslationCoversKnownModulePhrases(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`"already in the accounts directory"`,
		`"parked by the operator; the credential file is still on disk"`,
		`"no credential file found; the CLI may still hold a token in the OS keyring, which this module cannot read"`,
		`"conventional location for a hand-written key file (not created by any OpenRouter tool)"`,
		`"the executable this gateway would run for a request today"`,
		`[/^this account is marked dead after /i`,
		`"the stored access token has expired and is refreshed from the saved refresh token on the next request"`,
		`"disabled by the operator": "已被操作员停用"`,
		`"no access token": "没有访问令牌"`,
		`"access token expired": "访问令牌已过期"`,
		`[/^\d+\s+session dead$/i, "会话失效"]`,
		`[/^web\.tabbit\.com is rate limiting this session/i, "Tabbit 正在限制这个会话；请稍后再试"]`,
		`[/^web\.tabbit\.com answered;\s*/i, "Tabbit 已正常响应："]`,
		`[/^not checked yet;\s*/i, "尚未检查；请点击测试"]`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("中文状态翻译表没有覆盖模块固定说明：%s", want)
		}
	}
}
