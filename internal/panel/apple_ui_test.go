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
		".view:not([hidden]) { animation: uiViewIn",
		"@keyframes uiViewIn",
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
