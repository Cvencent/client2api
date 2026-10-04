package panel

import (
	"strings"
	"testing"
)

// TestPlatformsViewConfiguresRouting pins the "平台配置" page: a nav entry and
// a view that edits each platform's priority and model blacklist, saved through
// the config endpoint all the other pages use and applied with a hot reload.
func TestPlatformsViewConfiguresRouting(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, `data-view="platforms"`) {
		t.Error(`index.html has no nav entry for the platforms view (data-view="platforms")`)
	}
	if !strings.Contains(src, `id="view-platforms"`) {
		t.Error(`index.html has no platforms view section (id="view-platforms")`)
	}
	if !strings.Contains(src, `id="pfBody"`) {
		t.Error(`the platforms view has no body container (id="pfBody")`)
	}
	if !strings.Contains(src, `id="btnPfSave"`) {
		t.Error(`the platforms view has no save button (id="btnPfSave")`)
	}

	if !strings.Contains(src, `platforms: "平台配置"`) {
		t.Error("the platforms view is not titled in TITLES")
	}
	if !strings.Contains(src, `if (v === "platforms") renderPlatforms();`) {
		t.Error("go() does not render the platforms view")
	}

	render := poolStatsFuncBody(t, src, "renderPlatforms")
	for _, want := range []string{
		`api("/panel/api/config")`,
		`api("/panel/api/models")`,
		`priority`,
	} {
		if !strings.Contains(render, want) {
			t.Errorf("renderPlatforms does not mention %q", want)
		}
	}
	if !strings.Contains(poolStatsFuncBody(t, src, "pfDisabledOf"), "disabled_models") {
		t.Error("the platforms view never reads disabled_models from the config")
	}

	save := poolStatsFuncBody(t, src, "savePlatforms")
	for _, want := range []string{
		`api("/panel/api/config"`,
		`platforms`,
		`/panel/api/reload`,
	} {
		if !strings.Contains(save, want) {
			t.Errorf("savePlatforms does not mention %q", want)
		}
	}
	if !strings.Contains(src, `$("#btnPfSave").addEventListener`) {
		t.Error("the save button has no click handler")
	}
}

// TestPlatformsModelFilterKeepsCardVisible 钉住平台卡片里的「过滤模型名」：它只能
// 藏模型行，不能把整张平台卡片（连输入框）藏掉。曾经的 bug 是过滤无匹配时卡片
// 被 hidden，输入框跟着消失，用户既改不了也清不掉，看起来就像过滤坏了。
func TestPlatformsModelFilterKeepsCardVisible(t *testing.T) {
	src := poolStatsUISource(t)

	apply := poolStatsFuncBody(t, src, "pfApply")
	// 卡片显隐只由全局搜索/只看已禁用决定（byGlobal），不能再看被本地过滤收缩
	// 过的命中数（旧实现用 shown，本地过滤一空就把卡片藏了）。
	if !strings.Contains(apply, "el.hidden = rows > 0 && byGlobal === 0;") {
		t.Error("pfApply no longer keeps the platform card visible when only the local filter excludes every model")
	}
	if strings.Contains(apply, "shown === 0") {
		t.Error("pfApply still hides the whole platform card on a local-filter miss (regression)")
	}

	// 过滤无匹配时要有行内空态，而不是凭空消失。
	if !strings.Contains(src, `class="hint pfNoMatch"`) || !strings.Contains(src, "没有匹配的模型") {
		t.Error("the platforms view has no no-match hint for the model filter")
	}
	if !strings.Contains(apply, `querySelector(".pfNoMatch")`) {
		t.Error("pfApply never toggles the no-match hint")
	}

	// 按 Esc 清空卡片过滤框，省得手动删字。
	if !strings.Contains(src, `$("#pfBody").addEventListener("keydown"`) {
		t.Error("the platform model filter has no Escape-to-clear handler")
	}
}

// The platform page configures two ceilings: the platform-wide one and the
// per-account one.  Both must be rendered and saved, or the operator can set
// the account ceiling only by editing JSON.
func TestPlatformsViewConfiguresThePerAccountCeiling(t *testing.T) {
	src := poolStatsUISource(t)

	render := poolStatsFuncBody(t, src, "renderPlatforms")
	for _, want := range []string{`max_in_flight_per_account`, `pfMifAcct`} {
		if !strings.Contains(render, want) {
			t.Errorf("renderPlatforms does not mention %q", want)
		}
	}

	save := poolStatsFuncBody(t, src, "savePlatforms")
	for _, want := range []string{`pfMifAcct`, `max_in_flight_per_account`} {
		if !strings.Contains(save, want) {
			t.Errorf("savePlatforms does not mention %q", want)
		}
	}
}

// The low-balance guard is configured on the same page: a per-platform
// threshold that parks an account whose known balance is at or below it.
func TestPlatformsViewConfiguresTheReserveGuard(t *testing.T) {
	src := poolStatsUISource(t)

	render := poolStatsFuncBody(t, src, "renderPlatforms")
	for _, want := range []string{`reserve_credits`, `pfReserve`} {
		if !strings.Contains(render, want) {
			t.Errorf("renderPlatforms does not mention %q", want)
		}
	}

	save := poolStatsFuncBody(t, src, "savePlatforms")
	for _, want := range []string{`pfReserve`, `reserve_credits`} {
		if !strings.Contains(save, want) {
			t.Errorf("savePlatforms does not mention %q", want)
		}
	}
}

// The pool publishes "low_credit (16h0m0s left)", so the panel's note lookup
// has to strip that suffix before it can find the translated word.
func TestNoteCellStripsTheCooldownSuffix(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, `low_credit: "积分不足，已暂停"`) {
		t.Error("the panel has no translation for the low-balance park note")
	}
	body := poolStatsFuncBody(t, src, "noteCell")
	if !strings.Contains(body, `replace(/\s+\([^()]*left\)$/, "")`) {
		t.Error("noteCell does not strip the pool's \" (… left)\" suffix before looking the note up")
	}
}
