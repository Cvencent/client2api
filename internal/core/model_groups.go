package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ModelGroupMember names one platform-specific model id that participates in
// an operator-declared equivalence group.
type ModelGroupMember struct {
	Client string
	Model  string
}

// ModelGroup is one virtual model name and the concrete platform members that
// can satisfy it. PlatformPriorities overrides the global platform priority
// for members of this group only.
type ModelGroup struct {
	Members            []ModelGroupMember
	PlatformPriorities map[string]int
	// Builtin reports that this group came from the gateway's default GPT-6
	// catalogue rather than the operator's config. The panel renders it as
	// editable and can persist an override or suppress it explicitly.
	Builtin bool
}

// SetModelGroups replaces the operator-declared equivalence groups. The lookup
// table is rebuilt atomically so a reload never exposes a half-installed map.
func (r *Registry) SetModelGroups(groups map[string]ModelGroup) {
	next := make(map[string]ModelGroup, len(groups))
	for rawName, group := range groups {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if name == "" {
			continue
		}

		normalized := ModelGroup{}
		normalized.Builtin = group.Builtin
		for _, member := range group.Members {
			client := strings.TrimSpace(member.Client)
			model := strings.TrimSpace(member.Model)
			if client == "" || model == "" {
				continue
			}
			normalized.Members = append(normalized.Members, ModelGroupMember{
				Client: client,
				Model:  model,
			})
		}
		if len(normalized.Members) == 0 {
			continue
		}

		if len(group.PlatformPriorities) > 0 {
			normalized.PlatformPriorities = make(map[string]int, len(group.PlatformPriorities))
			for platform, priority := range group.PlatformPriorities {
				platform = strings.TrimSpace(platform)
				if platform != "" {
					normalized.PlatformPriorities[platform] = priority
				}
			}
			if len(normalized.PlatformPriorities) == 0 {
				normalized.PlatformPriorities = nil
			}
		}
		next[name] = normalized
	}

	lookup := make(map[string]string, len(next))
	names := make([]string, 0, len(next))
	for name := range next {
		names = append(names, name)
	}
	sort.Strings(names)

	// Group names win over member aliases. Iterating names in sorted order
	// makes collisions deterministic even when a hand-written config bypasses
	// the panel validator.
	for _, name := range names {
		addModelGroupLookup(lookup, name, name)
	}
	for _, name := range names {
		for _, member := range next[name].Members {
			for _, key := range []string{
				normalizeModelID(member.Model),
				normalizeModelID(modelDisplayID(member.Model)),
				canonicalModelID(member.Model),
			} {
				addModelGroupLookup(lookup, key, name)
			}
		}
	}

	r.mu.Lock()
	r.modelGroups = next
	r.modelGroupByLookup = lookup
	r.mu.Unlock()
}

func addModelGroupLookup(lookup map[string]string, key, name string) {
	if key == "" {
		return
	}
	if _, exists := lookup[key]; !exists {
		lookup[key] = name
	}
}

// ModelGroups returns a defensive copy of the configured groups.
func (r *Registry) ModelGroups() map[string]ModelGroup {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]ModelGroup, len(r.modelGroups))
	for name, group := range r.modelGroups {
		out[name] = cloneModelGroup(group)
	}
	return out
}

func cloneModelGroup(group ModelGroup) ModelGroup {
	out := ModelGroup{
		Members: append([]ModelGroupMember(nil), group.Members...),
		Builtin: group.Builtin,
	}
	if len(group.PlatformPriorities) > 0 {
		out.PlatformPriorities = make(map[string]int, len(group.PlatformPriorities))
		for platform, priority := range group.PlatformPriorities {
			out.PlatformPriorities[platform] = priority
		}
	}
	return out
}

func (r *Registry) modelGroupForLookup(model string) (string, ModelGroup, bool) {
	keys := []string{
		normalizeModelID(model),
		normalizeModelID(modelDisplayID(model)),
		canonicalModelID(model),
	}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}

		r.mu.RLock()
		name, ok := r.modelGroupByLookup[key]
		if !ok {
			r.mu.RUnlock()
			continue
		}
		group, ok := r.modelGroups[name]
		if ok {
			group = cloneModelGroup(group)
		}
		r.mu.RUnlock()
		if ok {
			return name, group, true
		}
	}
	return "", ModelGroup{}, false
}

func (r *Registry) resolveModelGroup(ctx context.Context, groupKey string, group ModelGroup) ([]Candidate, error) {
	owners := make([]Candidate, 0, len(group.Members))
	seen := make(map[string]struct{}, len(group.Members))
	autoPriority, autoPrioritySet := modelGroupPriority(group, "Auto")
	addOwner := func(c Client, model string, free bool, dynamicAuto bool) {
		key := strings.ToLower(c.Name()) + "\x00" + strings.ToLower(strings.TrimSpace(model))
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		owners = append(owners, Candidate{
			Client:          c,
			Model:           model,
			Free:            free,
			group:           groupKey,
			dynamicAuto:     dynamicAuto,
			autoPriority:    autoPriority,
			autoPrioritySet: autoPrioritySet,
		})
	}
	for _, member := range group.Members {
		// Auto/<model> is a dynamic member: it expands to every live platform
		// whose catalogue serves that model. This lets a group track custom
		// relay sources without naming each one in the config.
		if strings.EqualFold(strings.TrimSpace(member.Client), "Auto") {
			for _, c := range r.All() {
				models, err := c.Models(ctx)
				if err != nil {
					continue
				}
				matched, ok, _ := selectCatalogModel(models, member.Model, !strings.ContainsAny(member.Model, "/:"), func(m Model) bool {
					return r.ModelAllowed(c.Name(), m.ID)
				})
				if ok {
					addOwner(c, matched.ID, modelFree(matched), true)
				}
			}
			continue
		}
		c, ok := r.Get(member.Client)
		if !ok {
			continue
		}
		models, err := c.Models(ctx)
		if err != nil {
			continue
		}
		matched, ok, _ := selectCatalogModel(models, member.Model, false, func(m Model) bool {
			return r.ModelAllowed(c.Name(), m.ID)
		})
		if !ok {
			continue
		}
		addOwner(c, matched.ID, modelFree(matched), false)
	}
	if len(owners) == 0 {
		return nil, fmt.Errorf("model group %q has no available members", groupKey)
	}
	return r.rankCandidates(ctx, owners), nil
}

func (r *Registry) priorityFor(name, group string) int {
	if group != "" {
		r.mu.RLock()
		groupConfig, ok := r.modelGroups[group]
		if ok {
			priority, found := modelGroupPriority(groupConfig, name)
			r.mu.RUnlock()
			if found {
				return priority
			}
			return r.priority(name)
		}
		r.mu.RUnlock()
	}
	return r.priority(name)
}

func modelGroupPriority(group ModelGroup, platform string) (int, bool) {
	if priority, ok := group.PlatformPriorities[platform]; ok {
		return priority, true
	}
	for name, priority := range group.PlatformPriorities {
		if strings.EqualFold(name, platform) {
			return priority, true
		}
	}
	return 0, false
}
