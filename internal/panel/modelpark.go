package panel

import (
	"client2api/internal/core"
	"client2api/internal/modelpark"
)

// mergeModelParks folds the gateway's own model-scoped rate-limit memory into
// one module's status snapshot.  The panel renders Extra["model_cooldowns"], so
// this is what makes a 429 visible for a module that keeps only an
// account-level cooldown.
//
// A module's own parks win over the gateway's: the module knows whether a
// refusal was a timed window or a deterministic "this model is not available
// here", while the gateway only saw the HTTP status.  The gateway's parks fill
// the gaps and are dropped once the module reports the same model.
func mergeModelParks(st *core.Status, platform string, parks *modelpark.Store) {
	if st == nil || parks == nil {
		return
	}
	for i := range st.Accounts {
		acct := &st.Accounts[i]
		own, reported := accountModelParks(acct)
		live := parks.ForAccount(platform, acct.ID)
		if !reported && len(live) == 0 {
			continue
		}
		if acct.Extra == nil {
			acct.Extra = map[string]any{}
		}
		acct.Extra["model_cooldowns"] = mergeParkLists(own, live)
	}
}

// accountModelParks reads the module's own park list.  reported distinguishes
// "the module published an empty list" from "the module does not report
// per-model parks at all".
func accountModelParks(acct *core.AccountStatus) ([]core.ModelPark, bool) {
	if acct == nil || acct.Extra == nil {
		return nil, false
	}
	raw, ok := acct.Extra["model_cooldowns"]
	if !ok {
		return nil, false
	}
	switch list := raw.(type) {
	case []core.ModelPark:
		return append([]core.ModelPark(nil), list...), true
	case []any:
		out := make([]core.ModelPark, 0, len(list))
		for _, item := range list {
			if p, ok := item.(core.ModelPark); ok {
				out = append(out, p)
			}
		}
		return out, true
	}
	return nil, true
}

// mergeParkLists keeps the module's entry for a model and appends the
// gateway's entries for models the module did not mention.
func mergeParkLists(own, live []core.ModelPark) []core.ModelPark {
	seen := make(map[string]bool, len(own)+len(live))
	out := make([]core.ModelPark, 0, len(own)+len(live))
	for _, p := range own {
		if p.Model == "" || seen[p.Model] {
			continue
		}
		seen[p.Model] = true
		out = append(out, p)
	}
	for _, p := range live {
		if p.Model == "" || seen[p.Model] {
			continue
		}
		seen[p.Model] = true
		out = append(out, p)
	}
	return core.SortModelParks(out)
}
