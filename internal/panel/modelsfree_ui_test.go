package panel

import (
	"strings"
	"testing"
)

// TestModelsViewCanFilterToFreeModels pins the shell side of OpenRouter's free
// catalogue.
//
// The module already marks a model Extra["free"]=true from the live
// `GET /models` pricing (both prompt and completion zero), and the panel
// handler already relays it.  What was missing is the only part an operator
// needs: a way to find those rows among the ~460 the router returns, and a
// visible marker that a row is free.  These assertions are static because the
// shell is a string splice the Go compiler never checks.
func TestModelsViewCanFilterToFreeModels(t *testing.T) {
	src := poolStatsUISource(t)

	// 1. The control lives next to the other model filters.
	if !strings.Contains(src, `id="mdFreeOnly"`) {
		t.Error(`index.html has no free-only control (id="mdFreeOnly")`)
	}

	// 2. Every built row records the module's own free marker.
	render := poolStatsFuncBody(t, src, "renderModels")
	if !strings.Contains(render, "free: ") {
		t.Error("renderModels does not carry a free flag on each item")
	}
	// ...and a free row is visibly badged, not only filterable.
	if !strings.Contains(render, "免费") {
		t.Error("renderModels does not badge free models")
	}
	// The counter is incremented in the row loop; without this declaration
	// the whole page dies on the first free row with a ReferenceError.
	if !strings.Contains(render, "freeRows = 0") {
		t.Error("renderModels increments freeRows without declaring it")
	}

	// 3. The filter reads the checkbox and honours the flag.
	apply := poolStatsFuncBody(t, src, "mdApply")
	if !strings.Contains(apply, `$("#mdFreeOnly")`) {
		t.Error("mdApply does not read the free-only checkbox")
	}
	if !strings.Contains(apply, "it.free") {
		t.Error("mdApply does not filter on the free flag")
	}

	// 4. Toggling the checkbox must re-run the filter, like 只看上游 does.
	if !strings.Contains(src, `$("#mdFreeOnly").addEventListener("change", mdApply)`) {
		t.Error("toggling the free-only checkbox does not re-run mdApply")
	}
}
