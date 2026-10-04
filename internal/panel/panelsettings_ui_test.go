package panel

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// panel.package_detail_limit, from the input box to the rendered table
// ---------------------------------------------------------------------------
//
// The knob exists to stop the credits view from rendering several hundred
// expired batches every refresh.  Like every other pool/panel tuning knob in
// this repo it has to survive four hops, and a break at any one of them is
// invisible: the page still renders the box, still saves, still echoes the
// value back, and simply nothing changes.  Neither the Go compiler (strings)
// nor shell_test.go's id guard (it only proves the element exists) can see
// that, so the hops are pinned here by name.

func readPanelSource(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("reading %s: %v", path, err)
	}
	return string(raw)
}

// TestPackageDetailLimitReachesEveryLayer walks the knob from the markup to
// the config struct.
func TestPackageDetailLimitReachesEveryLayer(t *testing.T) {
	ui := poolStatsUISource(t)
	save := poolStatsFuncBody(t, ui, "saveConfig")

	// Hop 1: the page renders the box from the server's resolved value, and
	// saves it back.  The render must fall back to d.panel (the effective
	// value) rather than reading the file raw: an operator who never wrote
	// the key has nothing under config.panel, and an empty box reads as
	// "switched off" instead of "default".
	if !strings.Contains(ui, `cfgNum("cfgPanelPkgDetailLimit"`) {
		t.Error("renderConfig does not render the cfgPanelPkgDetailLimit input")
	}
	if !strings.Contains(ui, "d.panel") {
		t.Error("renderConfig does not read the resolved panel settings (d.panel)")
	}
	if !strings.Contains(save, `$("#cfgPanelPkgDetailLimit")`) {
		t.Error("saveConfig does not read the cfgPanelPkgDetailLimit input")
	}
	if !strings.Contains(save, "patch.panel = panel") {
		t.Error("saveConfig never writes the panel section into the patch")
	}
	// Only the empty string deletes: 0 is a legal value the server resolves
	// into the default, so treating it as "clear" would silently drop it.
	if !strings.Contains(save, `if ("package_detail_limit" in bPanel) panel.package_detail_limit = null;`) {
		t.Error("saveConfig does not delete the key on an empty box")
	}

	// Hop 2: cmd/client2api parses the key and threads it into the panel.
	mainSrc := readPanelSource(t, "../../cmd/client2api/main.go")
	if !strings.Contains(mainSrc, `json:"package_detail_limit"`) {
		t.Error("cmd/client2api does not declare the package_detail_limit config key")
	}
	if !strings.Contains(mainSrc, ".Panel.PackageDetailLimit") {
		t.Error("cmd/client2api never reads cfg.Panel.PackageDetailLimit")
	}
	if !strings.Contains(mainSrc, "PackageDetailLimit:") {
		t.Error("cmd/client2api does not pass PackageDetailLimit into panel.Options")
	}

	// Hop 3: the panel reports the resolved value on its own, next to the
	// redacted file, so the page never has to carry a second default.
	cfg := readPanelSource(t, "config.go")
	if !strings.Contains(cfg, "package_detail_limit") {
		t.Error("config.go does not serialise panel.package_detail_limit")
	}
	if !strings.Contains(cfg, "PackageDetailLimit: p.opts.PackageDetailLimit") {
		t.Error("configRead does not report the resolved PackageDetailLimit")
	}

	// Hop 4: the writer knows the section, so a save keeps it in place
	// instead of shuffling it to the end of the file, and validates it.
	write := readPanelSource(t, "configwrite.go")
	if !strings.Contains(write, `"panel"`) {
		t.Error("configKeyOrder does not know the panel section")
	}
	for _, want := range []string{
		"panel must be an object",
		"panel.package_detail_limit must be a number",
		"panel.package_detail_limit must be a whole number",
	} {
		if !strings.Contains(write, want) {
			t.Errorf("validateConfig is missing %q", want)
		}
	}
}

// TestConfigReadReportsTheResolvedPanelSettings proves hop 3 end to end: the
// number the panel was constructed with is the number the page receives,
// regardless of what the file happens to contain.
func TestConfigReadReportsTheResolvedPanelSettings(t *testing.T) {
	// The file says nothing about the panel section at all.
	path := configFile(t, `{"listen":"127.0.0.1:8788"}`)
	p := configPanel(path)
	p.opts.PackageDetailLimit = 7

	w, out := doConfig(t, p, http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET returned HTTP %d: %s", w.Code, w.Body.String())
	}
	ps, ok := out["panel"].(map[string]any)
	if !ok {
		t.Fatalf("config response has no panel object: %v", out)
	}
	if got := ps["package_detail_limit"]; got != float64(7) {
		t.Fatalf("panel.package_detail_limit = %v, want 7", got)
	}
}

// TestConfigPatchStoresThePackageDetailLimit covers the write side: the value
// lands on disk under the panel section, and null removes it again.
func TestConfigPatchStoresThePackageDetailLimit(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)

	mustSave(t, p, `{"panel":{"package_detail_limit":3}}`)
	on := onDisk(t, path)
	panelSec, ok := on["panel"].(map[string]any)
	if !ok {
		t.Fatalf("saved config has no panel object: %v", on)
	}
	if got := panelSec["package_detail_limit"]; got != float64(3) {
		t.Fatalf("saved panel.package_detail_limit = %v, want 3", got)
	}

	// null deletes the key, which is what an emptied box sends.
	mustSave(t, p, `{"panel":{"package_detail_limit":null}}`)
	panelSec, _ = onDisk(t, path)["panel"].(map[string]any)
	if _, present := panelSec["package_detail_limit"]; present {
		t.Fatalf("null did not delete the key: %v", panelSec)
	}
}

// TestConfigPatchRejectsAMalformedPackageDetailLimit pins the validator.  Note
// that 0 and negatives are NOT rejected: applyDefaults resolves them into the
// default, exactly as the reference's normalize() does, so writing 0 is a
// legal way to say "unset".
func TestConfigPatchRejectsAMalformedPackageDetailLimit(t *testing.T) {
	cases := []struct {
		name  string
		patch string
		want  string
	}{
		{"not an object", `{"panel":5}`, "panel must be an object"},
		{"not a number", `{"panel":{"package_detail_limit":"5"}}`, "must be a number"},
		{"fractional", `{"panel":{"package_detail_limit":2.5}}`, "must be a whole number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := configFile(t, baseConfig)
			p := configPanel(path)
			w, out := doConfig(t, p, http.MethodPatch, tc.patch)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("HTTP %d, want 400: %s", w.Code, w.Body.String())
			}
			msg, _ := out["error"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("error %q does not mention %q", msg, tc.want)
			}
		})
	}

	// Zero is accepted and persisted, because the resolver turns it into the
	// default rather than into an empty table.
	path := configFile(t, baseConfig)
	p := configPanel(path)
	mustSave(t, p, `{"panel":{"package_detail_limit":0}}`)
	panelSec, _ := onDisk(t, path)["panel"].(map[string]any)
	if got := panelSec["package_detail_limit"]; got != float64(0) {
		t.Fatalf("saved panel.package_detail_limit = %v, want 0", got)
	}
}

// TestCreditsViewCollapsesPackagesBeyondTheLimit pins the credits view's half
// of the feature: batches are ordered by earliest expiry, only the first N are
// shown, and the rest hide behind a clickable group header rather than being
// dropped.
func TestCreditsViewCollapsesPackagesBeyondTheLimit(t *testing.T) {
	src := poolStatsUISource(t)

	for _, want := range []string{
		"const PK_DEFAULT_DETAIL_LIMIT = 5;",
		"function pkDetailLimitValue(raw)",
		"function pkDetailLimit(body)",
		"function pkExpiryMs(p)",
		"function pkDetailCompare(a, b)",
		"function pkDetailGroups(packs, limit)",
		"function crMoreRow(group, text, count, size, remain)",
		`data-pk-group="`,
		`data-pk-row="`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the credits view is missing %q", want)
		}
	}

	// The hidden rows must actually start hidden, otherwise the collapse is
	// cosmetic and the page renders everything anyway.
	if !strings.Contains(src, `' class="pk-hidden-row" data-pk-row="' + esc(group) + '" hidden'`) {
		t.Error("hidden package rows are rendered without the hidden attribute")
	}

	// The click handler has to flip both the rows and the button label, and
	// it must scope its query to the table it was clicked in.
	body := poolStatsFuncBody(t, src, "renderCredits")
	if !strings.Contains(body, "pkDetailLimit") {
		t.Error("renderCredits does not consult the configured detail limit")
	}
	if !strings.Contains(src, `querySelectorAll('tr[data-pk-row="'`) {
		t.Error("no handler expands the collapsed rows")
	}
	if !strings.Contains(src, "aria-expanded") {
		t.Error("the group toggle never updates aria-expanded")
	}
}
