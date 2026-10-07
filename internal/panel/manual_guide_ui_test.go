package panel

import (
	"strings"
	"testing"
)

func TestManualAddExplainsItsPurposeAndSteps(t *testing.T) {
	src := poolStatsUISource(t)

	for _, want := range []string{
		`id="manGuide"`,
		`function manualGuideSteps(`,
		`function manualGuideHTML(`,
		`manualGuideHTML(n, fs)`,
		`"手动添加怎么用"`,
		`"只在浏览器登录或导入凭据拿不到时使用"`,
		`"浏览器登录"`,
		`"导入凭据"`,
		`"API Key"`,
		`"测试"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("manual add tutorial is missing %q", want)
		}
	}
}

func TestManualAddHideEmptyForm(t *testing.T) {
	body := poolStatsFuncBody(t, poolStatsUISource(t), "pickAddClient")

	for _, want := range []string{
		`const manualTab = $$("#addTabs .tab").find(x => x.dataset.tab === "manual");`,
		`const canManualTab = k.manage && (((k.fields || []).length > 0) || qcTargets(n).length > 0);`,
		`manualTab.hidden = !canManualTab;`,
		`if (!tabs.includes(ADD.tab)) setAddTab(tabs[0] || "login");`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("manual tab visibility is missing %q", want)
		}
	}
}
