package openaicompat

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateNeverSplitsARune pins issue #70: vendor refusals are Chinese,
// and a byte cut through a multi-byte character produced invalid UTF-8 that
// encoding/json turned into U+FFFD on the panel -- 积分余额不足 came back as
// 积分余�....  The cut is rune-aware now.
func TestTruncateNeverSplitsARune(t *testing.T) {
	long := strings.Repeat("积", 200)
	got := truncate(long, 100)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got[:30])
	}
	if !strings.HasPrefix(got, strings.Repeat("积", 100)) {
		t.Fatalf("truncate lost whole runes: %q", got[:30])
	}
	// A short string passes through untouched.
	if got := truncate("积分余额不足", 100); got != "积分余额不足" {
		t.Fatalf("truncate = %q, want the input unchanged", got)
	}
	// The count is in runes, not bytes: 3 runes survive a limit of 3 (this
	// module's truncate appends no ellipsis -- 300 bytes of refusal echo is
	// the budget, not a display line).
	if got := truncate("积分余额不足", 3); got != "积分余" {
		t.Fatalf("truncate(x, 3) = %q, want 积分余 (3 whole runes)", got)
	}
}
