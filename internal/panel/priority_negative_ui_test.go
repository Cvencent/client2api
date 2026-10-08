package panel

import (
	"strings"
	"testing"
)

func TestPlatformPriorityAcceptsNegativeIntegers(t *testing.T) {
	src := poolStatsUISource(t)

	render := poolStatsFuncBody(t, src, "renderPlatforms")
	for _, want := range []string{
		`class="cfgtext pfPrio"`,
		`title="可为负数；数字越小越优先，例如 -10 优先于 -5。留空使用默认 0"`,
	} {
		if !strings.Contains(render, want) {
			t.Errorf("renderPlatforms is missing %q", want)
		}
	}

	save := poolStatsFuncBody(t, src, "savePlatforms")
	for _, want := range []string{
		`const prio = raw === "" ? 0 : Number(raw);`,
		`!Number.isInteger(prio)`,
		`优先级必须是整数（可为负数）`,
	} {
		if !strings.Contains(save, want) {
			t.Errorf("savePlatforms is missing %q", want)
		}
	}
	if strings.Contains(save, `prio < 0`) {
		t.Error("savePlatforms still rejects negative platform priorities")
	}
}

func TestAccountPriorityCopyExplainsNegativeOrdering(t *testing.T) {
	src := poolStatsUISource(t)
	want := `title="可为负数；数字越小越优先，例如 -10 优先于 -5；同优先级继续轮询。留空恢复默认 0"`
	if !strings.Contains(src, want) {
		t.Errorf("account priority input is missing %q", want)
	}
	if !strings.Contains(src, `账号优先级必须是整数（可为负数）`) {
		t.Error("account priority validation copy does not explain negative values")
	}
}
