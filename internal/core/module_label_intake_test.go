package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestModuleLabelIntakesAreClamped 扫描每个模块的 accounts.go（issue #232）：
// 凡是出现了「从 AccountSpec 取 label」形状的模块，要么已走共享钳制
// （core.ClampLabel / spec.LabelOf），要么带着等价的本模块上限
// （codearts 的 sanitizeLabel、minimaxcode 的 truncateLabel——两者都截 120）。
// 新模块接 label 时漏掉钳制会在这里红。
func TestModuleLabelIntakesAreClamped(t *testing.T) {
	clientsDir := os.Getenv("CLIENT2API_CLIENTS_DIR")
	if clientsDir == "" {
		clientsDir = "../../clients"
	}
	entries, err := os.ReadDir(clientsDir)
	if err != nil {
		t.Skipf("cannot read clients dir: %v", err)
	}
	clampMarkers := []string{"core.ClampLabel", "spec.LabelOf", "LabelOf(\"label\"", "LabelOf(\"nickname\"", "sanitizeLabel(", "truncateLabel("}
	// An intake looks like one of these; a module that reads a label without
	// any clamp marker nearby is the regression this test exists for.
	intakeShapes := []string{
		`spec.Fields["label"]`,
		`spec.Field("label")`,
		`spec.Label`,
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		src, err := os.ReadFile(filepath.Join(clientsDir, e.Name(), "accounts.go"))
		if err != nil {
			continue // module without an accounts.go
		}
		code := string(src)
		readsLabel := false
		for _, shape := range intakeShapes {
			if strings.Contains(code, shape) {
				readsLabel = true
				break
			}
		}
		if !readsLabel {
			continue
		}
		clamped := false
		for _, m := range clampMarkers {
			if strings.Contains(code, m) {
				clamped = true
				break
			}
		}
		if !clamped {
			t.Errorf("clients/%s/accounts.go 读了 AccountSpec 的 label 却没有任何钳制（core.ClampLabel / LabelOf / 120 上限的 sanitize）：一次 20 万字符的粘贴会逐拍骑在 /panel/api/overview 上（issue #232）", e.Name())
		}
	}
}
