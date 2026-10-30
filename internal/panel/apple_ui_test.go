package panel

import (
	"strings"
	"testing"
)

// TestPanelUsesUnifiedAppleStyleDesignLayer pins the cross-page visual contract:
// one semantic token layer, a translucent shell, one motion family, and the
// accessibility fallbacks that keep the higher-density interface usable.
func TestPanelUsesUnifiedAppleStyleDesignLayer(t *testing.T) {
	src := poolStatsUISource(t)
	required := []string{
		"--radius-control:",
		"--radius-panel:",
		"--ease-smooth:",
		"backdrop-filter: blur(18px) saturate(180%)",
		"color-mix(in srgb, var(--surface) 78%, transparent)",
		".nav a.on::before",
		// 视图容器的整层淡入已经移除：它会和三套区块级编排叠在一起，每次切页
		// 整屏先透明再浮现，观感就是「闪一下」。现在入场由 .fx-in 负责，
		// 这里改成钉住那套编排本身，而不是被删掉的那条规则。
		".fx-in { animation: fxInUp",
		"@keyframes fxInUp",
		".subtabs {",
		".subtab.on",
		"button:focus-visible",
		"input:focus-visible",
		"@media (prefers-reduced-motion: reduce)",
		"@media (max-width: 760px)",
	}
	for _, want := range required {
		if !strings.Contains(src, want) {
			t.Errorf("unified panel design layer is missing %q", want)
		}
	}
}
