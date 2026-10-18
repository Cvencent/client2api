package core

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RoutePlanSource names the rule that decided a step's priority.  The static
// plan mirrors the runtime ResolveCandidates ranking without touching live
// account state: base is the platform's configured priority, schedule means a
// time-window override decided it, group means a model-group override, and
// auto_pool marks a platform inside a model-group Auto member's pool (ordered
// by its own platform priority when the group sets no Auto priority).
type RoutePlanSource string

const (
	RoutePlanSourceBase     RoutePlanSource = "base"
	RoutePlanSourceSchedule RoutePlanSource = "schedule"
	RoutePlanSourceGroup    RoutePlanSource = "group"
	RoutePlanSourceAutoPool RoutePlanSource = "auto_pool"
)

// RoutePlanStep is one platform in the failover chain.  Position is 1-based;
// a failure on platform N falls through to N+1.
type RoutePlanStep struct {
	Position   int             `json:"position"`
	Platform   string          `json:"platform"`
	ModelID    string          `json:"model_id"`
	Free       bool            `json:"free"`
	Priority   int             `json:"priority"`
	Source     RoutePlanSource `json:"priority_source"`
	SourceName string          `json:"priority_source_name"`
	// ContextLength is the effective input window the panel resolved for this
	// platform's concrete model. It travels with the route plan so operators
	// can compare failover candidates without opening the model table again.
	ContextLength int64  `json:"context_length,omitempty"`
	ContextSource string `json:"context_source,omitempty"`
	AutoPool      bool   `json:"auto_pool,omitempty"`
	Group         string `json:"group,omitempty"`
	// Preferred marks the member that matches the caller's spelling of a
	// bare group name: the runtime tries it before the group's siblings.
	// Writing client/model is a hard pin instead and yields that one
	// platform only, so this flag never survives a qualified request.
	Preferred   bool   `json:"preferred,omitempty"`
	ScheduleHit string `json:"schedule_hit,omitempty"`
}

// RoutePlan is the whole static resolution: the model after Auto/ stripping
// and alias resolution, plus the ordered platform chain.
type RoutePlan struct {
	Model string          `json:"model"`
	Steps []RoutePlanStep `json:"steps"`
}

// ResolveStaticCatalog answers "which platform would the router try first,
// and which one after that?" from a caller-supplied catalogue snapshot.  It
// never calls Client.Models or Client.Status, so the panel can show the
// priority chain without refreshing caches or touching account state.
//
// catalogs maps platform name to the catalogue snapshot the caller already
// has (the panel reuses its /panel/api/models data).  The resolution follows
// the same rules as the runtime: model groups win for non-Auto requests,
// Auto/ skips groups and searches the concrete catalogues directly, schedules
// override base priorities, and equal priorities fall back to free-first and
// then the platform name.
func (r *Registry) ResolveStaticCatalog(model string, catalogs map[string][]Model) (RoutePlan, error) {
	return r.ResolveStaticCatalogAt(model, catalogs, time.Now())
}

// ResolveStaticCatalogAt is ResolveStaticCatalog with an injectable clock, so
// tests (and a curious operator) can see what a schedule window will do.
func (r *Registry) ResolveStaticCatalogAt(model string, catalogs map[string][]Model, now time.Time) (RoutePlan, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return RoutePlan{}, fmt.Errorf("model is required")
	}

	// Aliases resolve the same way the runtime resolves them, before the
	// Auto/ strip and the group lookup.
	alias, hasAlias := r.Alias(model)
	if !hasAlias {
		alias, hasAlias = r.Alias(normalizeModelID(model))
	}
	if hasAlias {
		model = alias
	}

	auto := false
	if len(model) >= len("auto/") && strings.EqualFold(model[:len("auto/")], "auto/") {
		model = strings.TrimSpace(model[len("auto/"):])
		if model == "" {
			return RoutePlan{}, fmt.Errorf("model is required after Auto/")
		}
		auto = true
	}

	// An explicit "client/model" is a hard pin, honoured before the
	// model-group lookup exactly like the runtime: only the named platform
	// participates, even when the same id is also a group member.  A group is
	// the operator's failover chain for a bare name, never something that may
	// override the platform the caller picked.  Auto/ skips this, because
	// Auto/<id> is the gateway's own routing name.
	prefix, rest, qualified := strings.Cut(model, "/")
	hardPin := ""
	if !auto && qualified && prefix != "" && rest != "" {
		if _, found := r.Get(prefix); found {
			hardPin = prefix
		}
	}

	groupKey := ""
	group := ModelGroup{}
	inGroup := false
	if !auto && hardPin == "" {
		groupKey, group, inGroup = r.modelGroupForLookup(model)
	}

	var (
		steps   []RoutePlanStep
		blocked []string
	)
	for _, name := range r.Names() {
		models, ok := catalogs[name]
		if !ok || len(models) == 0 {
			continue
		}
		if hardPin != "" && !strings.EqualFold(name, hardPin) {
			continue
		}

		if inGroup {
			step, ok := r.planGroupMember(group, groupKey, name, models, model, now)
			if !ok {
				continue
			}
			if step == nil {
				// The member named a concrete model this platform lists but
				// blacklists; record it for the "disabled everywhere" error.
				blocked = append(blocked, name)
				continue
			}
			steps = append(steps, *step)
			continue
		}

		// Plain catalogue search (bare id, hard pin, or Auto/ request).  A
		// hard pin carries a platform prefix the catalogue does not repeat, so
		// the search uses the part after the slash, exactly like the runtime's
		// resolveQualifiedCandidate.  A slashed id never falls back to
		// namespace equivalents, matching the runtime's allowEquivalent rule.
		search := model
		if hardPin != "" {
			search = rest
		}
		m, ok, wasBlocked := selectCatalogModel(models, search, !strings.ContainsAny(search, "/:"), func(m Model) bool {
			return r.ModelAllowed(name, m.ID)
		})
		if !ok {
			if wasBlocked {
				blocked = append(blocked, name)
			}
			continue
		}
		step := RoutePlanStep{
			Platform:   name,
			ModelID:    m.ID,
			Free:       modelFree(m),
			Priority:   r.priorityAt(name, now),
			Source:     RoutePlanSourceBase,
			SourceName: "平台优先级",
		}
		step.ContextLength, step.ContextSource = routePlanContext(m)
		if hitName, hit := r.scheduleWindowSource(name, now); hit {
			step.Source = RoutePlanSourceSchedule
			step.SourceName = "分时规则"
			step.ScheduleHit = hitName
		}
		steps = append(steps, step)
	}

	sort.SliceStable(steps, func(i, j int) bool {
		a, b := steps[i], steps[j]
		if a.Preferred != b.Preferred {
			return a.Preferred
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.Free != b.Free {
			return a.Free
		}
		return a.Platform < b.Platform
	})
	for i := range steps {
		steps[i].Position = i + 1
	}

	if len(steps) == 0 {
		if len(blocked) > 0 {
			sort.Strings(blocked)
			return RoutePlan{}, fmt.Errorf("model %q is disabled on every client that serves it (%s); re-enable it in the platform configuration", model, strings.Join(blocked, ", "))
		}
		if hardPin == "" && qualified && prefix != "" && rest != "" && !auto {
			return RoutePlan{}, fmt.Errorf("unknown client %q in model %q, and no module lists that id; qualify it as <client>/%s", prefix, model, model)
		}
		return RoutePlan{}, fmt.Errorf("no client serves model %q; qualify it as <client>/%s", model, model)
	}
	return RoutePlan{Model: model, Steps: steps}, nil
}

// planGroupMember resolves one platform's slice of a model group.  The first
// configured member that names this platform (an Auto member names every
// platform) wins, mirroring the runtime's dedupe order.  ok=false means the
// platform is not part of the group's chain; a nil step with ok=true means
// the member matched but the platform blacklists the model.
func (r *Registry) planGroupMember(group ModelGroup, groupKey, name string, models []Model, requested string, now time.Time) (*RoutePlanStep, bool) {
	for _, member := range group.Members {
		client := strings.TrimSpace(member.Client)
		isAuto := strings.EqualFold(client, "Auto")
		if !isAuto && !strings.EqualFold(client, name) {
			continue
		}
		allowEquivalent := isAuto && !strings.ContainsAny(member.Model, "/:")
		m, ok, wasBlocked := selectCatalogModel(models, member.Model, allowEquivalent, func(m Model) bool {
			return r.ModelAllowed(name, m.ID)
		})
		if !ok {
			if wasBlocked {
				return nil, true
			}
			continue
		}

		step := RoutePlanStep{
			Platform: name,
			ModelID:  m.ID,
			Free:     modelFree(m),
			Group:    groupKey,
		}
		step.ContextLength, step.ContextSource = routePlanContext(m)
		// The Auto member expands to every platform that serves the model.
		// The runtime sorts the pool members by each platform's own priority
		// (a group-set Auto priority only positions the whole pool), so the
		// plan shows the platform priority as the effective intra-pool order
		// and marks every step auto_pool.
		step.Priority = r.priorityAt(name, now)
		step.Source = RoutePlanSourceAutoPool
		step.SourceName = "Auto 池"
		step.AutoPool = true
		if isAuto {
			return &step, true
		}
		step.Preferred = memberMatchesRequest(member, requested)
		if p, found := modelGroupPriority(group, name); found {
			step.Priority = p
			step.Source = RoutePlanSourceGroup
			step.SourceName = "模型路由组"
			step.AutoPool = false
			return &step, true
		}
		if hitName, hit := r.scheduleWindowSource(name, now); hit {
			step.Source = RoutePlanSourceSchedule
			step.SourceName = "分时规则"
			step.ScheduleHit = hitName
			step.AutoPool = false
		}
		return &step, true
	}
	return nil, false
}

// routePlanContext reads the context window a panel catalogue item carries.
// The panel normally sends the already-resolved effective value as
// context_length plus its provenance, but accepting the module spellings too
// keeps the static plan useful for callers that supply a raw catalogue.
func routePlanContext(m Model) (int64, string) {
	if m.Extra == nil {
		return 0, ""
	}
	var length int64
	for _, key := range []string{"context_length", "context_window", "max_input_tokens"} {
		if n := routePlanInt64(m.Extra[key]); n > 0 {
			length = n
			break
		}
	}
	source, _ := m.Extra["context_source"].(string)
	return length, strings.TrimSpace(source)
}

func routePlanInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case uint:
		if uint64(n) <= uint64(1<<63-1) {
			return int64(n)
		}
	case uint32:
		return int64(n)
	case uint64:
		if n <= uint64(1<<63-1) {
			return int64(n)
		}
	case float32:
		return int64(n)
	case float64:
		return int64(n)
	case json.Number:
		if n, err := n.Int64(); err == nil {
			return n
		}
	}
	return 0
}

// scheduleWindowSource reports whether a priority window covers now and, if
// so, which one (1-based) so the panel can name the rule.
func (r *Registry) scheduleWindowSource(name string, now time.Time) (string, bool) {
	r.mu.RLock()
	pol := r.platforms[name]
	r.mu.RUnlock()
	local := now.In(platformPriorityCST)
	minute := local.Hour()*60 + local.Minute()
	for i, w := range pol.prioritySchedule {
		if priorityWindowContains(w, minute) {
			return fmt.Sprintf("窗口%d", i+1), true
		}
	}
	return "", false
}
