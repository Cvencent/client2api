package core

import (
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// #236/#237 的 core 层钉子。
// ---------------------------------------------------------------------------

// TestLogNewCatalogueIDsAnnouncesOnlyFreshIDs 钉住 #237：差集按大小写不敏感
// 对比、只报新增、无新增时零日志（轮询的常态是安静）。
func TestLogNewCatalogueIDsAnnouncesOnlyFreshIDs(t *testing.T) {
	var lines []string
	logf := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	previous := []Model{{ID: "old-a"}, {ID: "Old-B"}}
	current := []Model{{ID: "old-a"}, {ID: "old-b"}, {ID: "new-c"}, {ID: "new-d"}}

	LogNewCatalogueIDs(logf, "test", previous, current)
	if len(lines) != 1 {
		t.Fatalf("新增两个模型时日志 %d 条，期望 1（issue #237）", len(lines))
	}
	for _, want := range []string{"2 new model", "new-c", "new-d"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("日志缺 %q：%s", want, lines[0])
		}
	}
	// 大小写不敏感：Old-B 与 old-b 是同一个，不得重复报告。
	if strings.Contains(lines[0], "Old-B") {
		t.Errorf("旧模型被当新报：%s（大小写不敏感差集，issue #237）", lines[0])
	}
	// 无新增：零日志。
	lines = nil
	LogNewCatalogueIDs(logf, "test", current, current)
	if len(lines) != 0 {
		t.Errorf("无新增也打了日志：%v（轮询常态必须安静）", lines)
	}
}
