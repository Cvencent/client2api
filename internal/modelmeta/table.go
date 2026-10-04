package modelmeta

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
)

// The static table ships inside the binary: an offline fallback that cannot fail
// to load, cannot be tampered with at runtime, and costs no I/O on a cold start.
// Every number in it is ported verbatim from an authoritative source and carries
// an `origin` in the file; an empty field means "no source", which is a correct
// value, while a guessed field would be a bug.
//
//go:embed table.json
var tableJSON []byte

// Realm names. Context and output limits are realm-independent (the reference
// keeps one context table because the same model id has the same window) while
// reasoning efforts are deliberately realm-split: for example the international
// edition of deepseek-v4.1-flash accepts only ["high"] and rejects low/max with an
// HTTP 400. The reference treats every realm except "global" as the CN table.
const (
	RealmCN     = "cn"
	RealmGlobal = "global"
)

// NormalizeRealm maps a realm name to the key used by the static table: anything
// that is not exactly "global" resolves to "cn", mirroring the reference's
// realmKey/staticEffortCap pair so an empty or unknown realm still gets the CN
// fallback vocabulary.
func NormalizeRealm(realm string) string {
	if strings.EqualFold(strings.TrimSpace(realm), RealmGlobal) {
		return RealmGlobal
	}
	return RealmCN
}

// effortSpec is one realm's reasoning-effort vocabulary.
type effortSpec struct {
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"default_effort,omitempty"`
}

// tableEntry is one static table row. Every field is optional on purpose: a row
// may carry only efforts (the reference's effort-only ids such as hy3-x), and a
// row may carry a context window with no output limit (auto, kimi-k2.8-preview),
// in which case the output limit stays empty rather than being invented.
type tableEntry struct {
	ContextLength   int64                 `json:"context_length,omitempty"`
	MaxOutputTokens int64                 `json:"max_output_tokens,omitempty"`
	Efforts         []string              `json:"efforts,omitempty"`
	DefaultEffort   string                `json:"default_effort,omitempty"`
	EffortsByRealm  map[string]effortSpec `json:"efforts_by_realm,omitempty"`
	Origin          string                `json:"origin,omitempty"`
	EffortOrigin    string                `json:"effort_origin,omitempty"`
}

// clientTable is one client's section of the table.
type clientTable struct {
	Models map[string]tableEntry `json:"models"`
}

// staticTable is the parsed, indexed table: an exact-match index plus a
// case-insensitive one. Model ids are case-sensitive in general, but some clients
// list them capitalised (tabbit lists "GPT-5.5" for the same model the reference
// calls "gpt-5.5"), so an exact miss falls back to a folded lookup.
type staticTable struct {
	exact  map[string]map[string]tableEntry
	folded map[string]map[string]tableEntry
	counts map[string]int
}

var (
	staticOnce sync.Once
	staticTab  *staticTable
)

// static returns the parsed table. It is parsed once, lazily, and a parse failure
// degrades to an empty table instead of panicking in a request path — the table is
// embedded, so a failure here is a build-time mistake caught by the package tests,
// not a runtime condition a caller should have to handle.
func static() *staticTable {
	staticOnce.Do(func() {
		staticTab = parseStaticTable(tableJSON)
	})
	return staticTab
}

func parseStaticTable(raw []byte) *staticTable {
	st := &staticTable{
		exact:  map[string]map[string]tableEntry{},
		folded: map[string]map[string]tableEntry{},
		counts: map[string]int{},
	}
	// The document carries top-level "_comment" metadata whose value is not a
	// client section, so decode the sections individually and skip them.
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sections); err != nil {
		return st
	}
	for client, blob := range sections {
		if strings.HasPrefix(client, "_") {
			continue
		}
		var ct clientTable
		if err := json.Unmarshal(blob, &ct); err != nil {
			// A malformed section is dropped rather than poisoning the others.
			continue
		}
		if len(ct.Models) == 0 {
			continue
		}
		exact := make(map[string]tableEntry, len(ct.Models))
		folded := make(map[string]tableEntry, len(ct.Models))
		for id, entry := range ct.Models {
			exact[id] = entry
			key := strings.ToLower(id)
			// An exact-name duplicate wins the folded slot, so the lookup stays
			// deterministic regardless of map iteration order.
			if _, dup := folded[key]; !dup {
				folded[key] = entry
			}
		}
		st.exact[client] = exact
		st.folded[client] = folded
		st.counts[client] = len(exact)
	}
	return st
}

// entry looks up one model for one client: exact id first, then case-insensitively.
func (st *staticTable) entry(client, model string) (tableEntry, bool) {
	if st == nil || model == "" {
		return tableEntry{}, false
	}
	if exact, ok := st.exact[client]; ok {
		if e, found := exact[model]; found {
			return e, true
		}
	}
	if folded, ok := st.folded[client]; ok {
		if e, found := folded[strings.ToLower(model)]; found {
			return e, true
		}
	}
	return tableEntry{}, false
}

// meta projects a row into a Meta for the given realm, tagging every field it
// emits with SourceStatic so provenance survives a later merge.
func (e tableEntry) meta(realm string) Meta {
	m := Meta{Source: SourceStatic, FieldSources: map[string]string{}}
	if e.ContextLength > 0 {
		m.ContextLength = e.ContextLength
		m.FieldSources[FieldContextLength] = SourceStatic
	}
	if e.MaxOutputTokens > 0 {
		m.MaxOutputTokens = e.MaxOutputTokens
		m.FieldSources[FieldMaxOutputTokens] = SourceStatic
	}
	efforts, def := e.effortsFor(realm)
	if len(efforts) > 0 {
		m.Efforts = efforts
		m.FieldSources[FieldEfforts] = SourceStatic
		if def != "" {
			m.DefaultEffort = def
			m.FieldSources[FieldDefaultEffort] = SourceStatic
		}
	}
	return m
}

// effortsFor picks the realm's vocabulary. A realm absent from a realm-split row
// yields nothing: the reference keeps two separate maps and a model missing from
// the global map simply has no global vocabulary, which must not be invented from
// the CN row.
func (e tableEntry) effortsFor(realm string) ([]string, string) {
	if len(e.EffortsByRealm) > 0 {
		spec, ok := e.EffortsByRealm[NormalizeRealm(realm)]
		if !ok {
			return nil, ""
		}
		return dedupeStrings(spec.Efforts), spec.DefaultEffort
	}
	return dedupeStrings(e.Efforts), e.DefaultEffort
}

// Clients returns the sorted client keys present in the static table.
func Clients() []string {
	st := static()
	out := make([]string, 0, len(st.counts))
	for client, n := range st.counts {
		if n > 0 {
			out = append(out, client)
		}
	}
	return sortedCopy(out)
}

// StaticModelCount returns how many static table entries a client has. Zero is a
// valid, expected answer: it means no source exists for that client's ids yet.
func StaticModelCount(client string) int {
	return static().counts[client]
}

// StaticModelIDs returns the sorted ids a client has static entries for.
func StaticModelIDs(client string) []string {
	st := static()
	exact, ok := st.exact[client]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(exact))
	for id := range exact {
		out = append(out, id)
	}
	return sortedCopy(out)
}

// StaticMeta resolves a model straight from the static table, bypassing the
// remote and cache layers. It is exported for the panel and for tests that need to
// assert on the table alone.
func StaticMeta(client, realm, model string) (Meta, bool) {
	e, ok := static().entry(client, model)
	if !ok {
		return Meta{}, false
	}
	m := e.meta(realm)
	if m.IsZero() {
		return Meta{}, false
	}
	return m, true
}
