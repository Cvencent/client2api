package panel

import "testing"

func TestConfigPatchAcceptsNegativePlatformAndAccountPriorities(t *testing.T) {
	path := configFile(t, baseConfig)
	out := mustSave(t, configPanel(path),
		`{"platforms":{"alpha":{"priority":-10,"account_priorities":{"minus-five":-5,"minus-ten":-10,"default":0}}}}`)

	changed, _ := out["changed"].([]any)
	if len(changed) != 1 || changed[0] != "platforms" {
		t.Fatalf("changed = %v, want [platforms]", out["changed"])
	}

	got := onDisk(t, path)
	pl, _ := got["platforms"].(map[string]any)
	alpha, _ := pl["alpha"].(map[string]any)
	if alpha["priority"] != float64(-10) {
		t.Fatalf("alpha priority = %v, want -10", alpha["priority"])
	}
	aps, _ := alpha["account_priorities"].(map[string]any)
	if aps["minus-five"] != float64(-5) || aps["minus-ten"] != float64(-10) || aps["default"] != float64(0) {
		t.Fatalf("account priorities = %v, want -5/-10/0", aps)
	}
}
