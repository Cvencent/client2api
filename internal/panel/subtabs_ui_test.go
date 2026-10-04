package panel

import (
	"regexp"
	"strings"
	"testing"
)

// subtabNavHTML returns the one nav row for a reusable secondary-tab group.
func subtabNavHTML(t *testing.T, src, key string) string {
	t.Helper()
	marker := `data-subtabs="` + key + `"`
	at := strings.Index(src, marker)
	if at < 0 {
		t.Fatalf("index.html has no secondary-tab group %q", key)
	}
	open := strings.LastIndex(src[:at], "<div")
	if open < 0 {
		t.Fatalf("secondary-tab group %q is not inside a div", key)
	}
	end := strings.Index(src[at:], "</div>")
	if end < 0 {
		t.Fatalf("secondary-tab group %q is unterminated", key)
	}
	return src[open : at+end+len("</div>")]
}

// subtabPanelHTML returns one panel up to the next panel in the same view.
func subtabPanelHTML(t *testing.T, src, name string) string {
	t.Helper()
	marker := `data-subtab-panel="` + name + `"`
	at := strings.Index(src, marker)
	if at < 0 {
		t.Fatalf("index.html has no secondary-tab panel %q", name)
	}
	open := strings.LastIndex(src[:at], "<div")
	if open < 0 {
		t.Fatalf("secondary-tab panel %q is not inside a div", name)
	}
	nextAt := strings.Index(src[at+len(marker):], `data-subtab-panel="`)
	sectionAt := strings.Index(src[at:], "</section>")
	if sectionAt < 0 {
		t.Fatalf("secondary-tab panel %q is not inside a section", name)
	}
	if nextAt >= 0 {
		nextAt += at + len(marker)
		if nextAt < sectionAt+at {
			if nextOpen := strings.LastIndex(src[:nextAt], "<div"); nextOpen >= open {
				return src[open:nextOpen]
			}
		}
	}
	return src[open : at+sectionAt]
}

func subtabOpenTag(t *testing.T, src, name string) string {
	t.Helper()
	marker := `data-subtab-panel="` + name + `"`
	at := strings.Index(src, marker)
	if at < 0 {
		t.Fatalf("index.html has no secondary-tab panel %q", name)
	}
	open := strings.LastIndex(src[:at], "<div")
	end := strings.Index(src[at:], ">")
	if open < 0 || end < 0 {
		t.Fatalf("secondary-tab panel %q has no opening tag", name)
	}
	return src[open : at+end+1]
}

func TestUsagePageHasThreeSecondaryTabs(t *testing.T) {
	src := poolStatsUISource(t)
	nav := subtabNavHTML(t, src, "usage")
	for _, want := range []string{
		`data-subtab="overview"`, ">总览<",
		`data-subtab="recent"`, ">最近调用<",
		`data-subtab="stats"`, ">统计明细<",
		`role="tab"`, `aria-selected="true"`, `class="chip subtab on"`,
	} {
		if !strings.Contains(nav, want) {
			t.Errorf("用量二级 Tab 缺少 %q", want)
		}
	}
	if got := strings.Count(nav, `class="chip subtab on"`); got != 1 {
		t.Errorf("用量二级 Tab 默认选中项 = %d, want exactly 1", got)
	}

	for _, tc := range []struct {
		panel string
		ids   []string
	}{
		{"usage-overview", []string{`id="usStats"`, `id="usChart"`}},
		{"usage-recent", []string{`id="usRecentFilter"`, `id="usRecentBody"`, `id="btnUsageRecentRefresh"`}},
		{"usage-stats", []string{`id="usClientBody"`, `id="usAccBody"`, `id="usModelBody"`, `id="usRealmBody"`}},
	} {
		panel := subtabPanelHTML(t, src, tc.panel)
		for _, id := range tc.ids {
			if !strings.Contains(panel, id) {
				t.Errorf("用量面板 %s 不包含 %s", tc.panel, id)
			}
		}
	}
	if strings.Contains(subtabOpenTag(t, src, "usage-overview"), " hidden") {
		t.Error("用量总览应该是默认可见面板")
	}
	for _, name := range []string{"usage-recent", "usage-stats"} {
		if !strings.Contains(subtabOpenTag(t, src, name), " hidden") {
			t.Errorf("用量面板 %s 默认应隐藏", name)
		}
	}
}

func TestTasksCenterHasThreeSecondaryTabs(t *testing.T) {
	src := poolStatsUISource(t)
	nav := subtabNavHTML(t, src, "taskscenter")
	for _, want := range []string{
		`data-subtab="schedule"`, ">定时任务<",
		`data-subtab="batch"`, ">批量执行<",
		`data-subtab="board"`, ">任务看板<",
		`role="tab"`, `aria-selected="true"`, `class="chip subtab on"`,
	} {
		if !strings.Contains(nav, want) {
			t.Errorf("任务中心二级 Tab 缺少 %q", want)
		}
	}
	if got := strings.Count(nav, `class="chip subtab on"`); got != 1 {
		t.Errorf("任务中心二级 Tab 默认选中项 = %d, want exactly 1", got)
	}

	for _, tc := range []struct {
		panel string
		ids   []string
	}{
		{"tasks-schedule", []string{`id="scBody"`, `id="scRunsBody"`, `id="btnScRunsReload"`}},
		{"tasks-batch", []string{`id="qcList"`, `id="ckList"`, `id="btnScanAll"`, `id="btnCkRun"`}},
		{"tasks-board", []string{`id="tbBody"`, `id="btnTbReload"`}},
	} {
		panel := subtabPanelHTML(t, src, tc.panel)
		for _, id := range tc.ids {
			if !strings.Contains(panel, id) {
				t.Errorf("任务中心面板 %s 不包含 %s", tc.panel, id)
			}
		}
	}
	if strings.Contains(subtabOpenTag(t, src, "tasks-schedule"), " hidden") {
		t.Error("定时任务应该是默认可见面板")
	}
	for _, name := range []string{"tasks-batch", "tasks-board"} {
		if !strings.Contains(subtabOpenTag(t, src, name), " hidden") {
			t.Errorf("任务中心面板 %s 默认应隐藏", name)
		}
	}
}

func TestSecondaryTabsUseOneReusableAccessibleController(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, "const SUBTABS = new Map();") {
		t.Fatal("缺少可复用的二级 Tab 控制器注册表")
	}
	body := poolStatsFuncBody(t, src, "setupSubtabs")
	for _, want := range []string{
		`data-subtab-panel`,
		`classList.toggle("on"`,
		`aria-selected`,
		`tabIndex`,
		`ev.key === "ArrowLeft"`,
		`ev.key === "ArrowRight"`,
		`ev.key === "Home"`,
		`ev.key === "End"`,
		`onShow(name)`,
		`SUBTABS.set(key, api)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("setupSubtabs 缺少 %q", want)
		}
	}
}

func TestSecondaryTabsKeepTheExistingRefreshHooks(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		`setupSubtabs("usage"`,
		`setupSubtabs("taskscenter"`,
		`$("#btnUsageRecentRefresh").addEventListener("click", () => renderUsage())`,
		"renderUsage()",
		"renderSchedule()",
		"renderQC()",
		"renderTaskBoard()",
		"reattachQueueView()",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("二级 Tab 接线缺少 %q", want)
		}
	}
	// The reusable controller must not bypass the existing render functions.
	if regexp.MustCompile(`function setupSubtabs\([^)]*\)[^{]*\{[^}]*innerHTML`).MatchString(src) {
		t.Error("setupSubtabs 不应自己渲染业务内容")
	}
}

// subtabSectionHTML returns the <section class="view"> that owns a secondary-tab group.
func subtabSectionHTML(t *testing.T, src, key string) string {
	t.Helper()
	marker := `id="view-` + key + `"`
	at := strings.Index(src, marker)
	if at < 0 {
		t.Fatalf("index.html has no view section %q", "view-"+key)
	}
	open := strings.LastIndex(src[:at], "<section")
	if open < 0 {
		t.Fatalf("view %q is not inside a section", key)
	}
	end := strings.Index(src[at:], "</section>")
	if end < 0 {
		t.Fatalf("view %q section is unterminated", key)
	}
	return src[open : at+end+len("</section>")]
}

// panelOpenTagByID returns the opening <div ...> tag whose id attribute is id.
func panelOpenTagByID(t *testing.T, src, id string) string {
	t.Helper()
	marker := `id="` + id + `"`
	at := strings.Index(src, marker)
	if at < 0 {
		return ""
	}
	open := strings.LastIndex(src[:at], "<div")
	if open < 0 {
		return ""
	}
	end := strings.Index(src[at:], ">")
	if end < 0 {
		return ""
	}
	return src[open : at+end+1]
}

// The controller selects a panel by the clicked tab's aria-controls, so every tab
// must point at a real tabpanel that is labelled by that same tab. Static markup
// drift here would silently show the wrong panel (or none) at runtime.
func TestSecondaryTabsWireEachTabToItsPanel(t *testing.T) {
	src := poolStatsUISource(t)
	for _, key := range []string{"usage", "taskscenter"} {
		section := subtabSectionHTML(t, src, key)
		tabs := regexp.MustCompile(`id="([^"]+)"[^>]*role="tab"[^>]*aria-controls="([^"]+)"`).FindAllStringSubmatch(section, -1)
		if len(tabs) != 3 {
			t.Fatalf("%s: found %d tab controls, want 3", key, len(tabs))
		}
		for _, m := range tabs {
			tabID, panelID := m[1], m[2]
			tag := panelOpenTagByID(t, section, panelID)
			if tag == "" {
				t.Errorf("%s: tab %q controls %q but no panel carries that id", key, tabID, panelID)
				continue
			}
			if !strings.Contains(tag, `role="tabpanel"`) {
				t.Errorf("%s: panel %q is missing role=tabpanel", key, panelID)
			}
			if !strings.Contains(tag, `aria-labelledby="`+tabID+`"`) {
				t.Errorf("%s: panel %q is not aria-labelledby tab %q", key, panelID, tabID)
			}
		}
	}
}
