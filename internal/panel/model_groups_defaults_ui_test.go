package panel

import (
	"strings"
	"testing"
)

func TestModelGroupUIExposesBuiltInDefaults(t *testing.T) {
	src := poolStatsUISource(t)

	render := poolStatsFuncBody(t, src, "renderPlatforms")
	for _, want := range []string{`model_group_defaults`, `pfModelGroupsHTML`} {
		if !strings.Contains(render, want) {
			t.Errorf("renderPlatforms does not pass %q into the model-group editor", want)
		}
	}

	group := poolStatsFuncBody(t, src, "pfModelGroupHTML")
	for _, want := range []string{`builtin`, `pfgBuiltin`, `pfGDel`} {
		if !strings.Contains(group, want) {
			t.Errorf("pfModelGroupHTML does not expose built-in state %q", want)
		}
	}
}

func TestModelGroupUIProtectsBuiltInNamesFromAccidentalDelete(t *testing.T) {
	src := poolStatsUISource(t)
	group := poolStatsFuncBody(t, src, "pfModelGroupHTML")
	for _, want := range []string{`builtin ?`, `readonly`, `!builtin`} {
		if !strings.Contains(group, want) {
			t.Errorf("pfModelGroupHTML does not protect built-in group names with %q", want)
		}
	}
}
