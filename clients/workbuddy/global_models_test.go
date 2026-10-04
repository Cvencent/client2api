package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// v3ConfigBody builds a /v3/config answer in the object form the real endpoint
// uses (models keyed by id).
func v3ConfigBody(ids ...string) string {
	models := map[string]any{}
	for _, id := range ids {
		models[id] = map[string]any{
			"id":              id,
			"name":            "Name " + id,
			"maxInputTokens":  128000,
			"maxOutputTokens": 8192,
		}
	}
	raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"models": models}})
	return string(raw)
}

// globalEnterpriseBody builds an enterprise probe answer: full entries under
// data.models, the shape the reference parses first.
func globalEnterpriseBody(ids ...string) string {
	entries := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, map[string]any{
			"id":              id,
			"name":            "Name " + id,
			"maxInputTokens":  200000,
			"maxOutputTokens": 8192,
			"reasoning": map[string]any{
				"supportedEfforts": []string{"high", "low"},
				"effort":           "high",
			},
		})
	}
	raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"models": entries}})
	return string(raw)
}

func TestWorkbuddyGlobalModelsRefusesTheCNRealmWithoutARequest(t *testing.T) {
	// Every probe route answers, so a single request would be visible.
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == v3ConfigPath {
			return jsonResponse(200, v3ConfigBody("global-1")), nil
		}
		return jsonResponse(200, globalEnterpriseBody("global-1")), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())
	a := wbCNAuth(t, c)

	if got := c.up.FetchGlobalModels(a); got != nil {
		t.Fatalf("FetchGlobalModels on CN = %v, want nil", got)
	}
	if got := c.up.FetchGlobalModelInfos(a); got != nil {
		t.Fatalf("FetchGlobalModelInfos on CN = %v, want nil", got)
	}
	if len(rt.calls) != 0 {
		t.Fatalf("the CN realm made %d requests: %v", len(rt.calls), wbPaths(rt))
	}
}

// countEnterpriseProbes counts the requests that went to either enterprise
// catalogue path.  Reading rt.calls is safe here and in the test below: every
// caller waits for its probes to finish before it returns, so no handler can be
// running when the count is taken.
func countEnterpriseProbes(rt *fakeRT) int {
	n := 0
	for _, p := range wbPaths(rt) {
		if p == globalModelsProbePaths[0] || p == globalModelsProbePaths[1] {
			n++
		}
	}
	return n
}

// TestWorkbuddyFetchModelsServesTheGlobalCatalogueFromItsCache is the reason the
// global catalogue exists.  The cache, its TTL and its failure cooldown were
// ported and tested, but for a while nothing on the production path called them:
// FetchModels probed the vendor's enterprise endpoint directly, so every panel
// refresh re-asked the vendor and the cache was dead code.  Counting probes
// across two calls is what distinguishes "the cache works" from "the cache is
// never consulted" -- a unit test on the cache itself cannot tell the two apart.
func TestWorkbuddyFetchModelsServesTheGlobalCatalogueFromItsCache(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case v3ConfigPath:
			return jsonResponse(200, v3ConfigBody("global-1")), nil
		}
		return jsonResponse(200, globalEnterpriseBody("global-1")), nil
	}}
	c, _ := panelClient(t, rt, intlAccountFiles())
	a := wbIntlAuth(t, c)

	first, err := c.up.FetchModels(a)
	if err != nil {
		t.Fatalf("first FetchModels: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("the first FetchModels listed no models")
	}
	afterFirst := countEnterpriseProbes(rt)
	if afterFirst == 0 {
		t.Fatal("the first FetchModels never probed the enterprise catalogue; the fixture is not exercising the global path")
	}

	// The /v3/config half is deliberately not cached by the global catalogue,
	// so only the enterprise half is counted here.
	second, err := c.up.FetchModels(a)
	if err != nil {
		t.Fatalf("second FetchModels: %v", err)
	}
	if len(second) == 0 {
		t.Fatal("the second FetchModels listed no models")
	}
	if after := countEnterpriseProbes(rt); after != afterFirst {
		t.Fatalf("the second FetchModels made %d more enterprise probe(s) (paths: %v); the global catalogue is not on the production path",
			after-afterFirst, wbPaths(rt))
	}
}

func TestWorkbuddyGlobalModelsPrefersTheV2EnterprisePath(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case v3ConfigPath:
			return jsonResponse(http.StatusInternalServerError, `{"msg":"down"}`), nil
		case "/v2/enterprises/personal/models":
			return jsonResponse(200, globalEnterpriseBody("v2-first", "v2-second")), nil
		case "/console/enterprises/personal/models":
			return jsonResponse(200, globalEnterpriseBody("console-only")), nil
		}
		return jsonResponse(http.StatusNotFound, `{}`), nil
	}}
	c, _ := panelClient(t, rt, intlAccountFiles())
	a := wbIntlAuth(t, c)

	got := c.up.FetchGlobalModels(a)
	if join := strings.Join(got, ","); join != "v2-first,v2-second" {
		t.Fatalf("names = %q, want the /v2 answer in order", join)
	}
	if n := wbCalls(rt, "/console/enterprises/personal/models"); n != 0 {
		t.Fatalf("the console path was probed %d times although /v2 answered", n)
	}
	if n := wbCalls(rt, "/v2/enterprises/personal/models"); n != 1 {
		t.Fatalf("/v2 probed %d times, want 1", n)
	}
}

func TestWorkbuddyGlobalModelsFallsBackToTheConsolePath(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case v3ConfigPath:
			return jsonResponse(http.StatusInternalServerError, `{"msg":"down"}`), nil
		case "/v2/enterprises/personal/models":
			return jsonResponse(http.StatusNotFound, `{}`), nil
		case "/console/enterprises/personal/models":
			return jsonResponse(200, globalEnterpriseBody("console-only")), nil
		}
		return jsonResponse(http.StatusNotFound, `{}`), nil
	}}
	c, _ := panelClient(t, rt, intlAccountFiles())

	got := c.up.FetchGlobalModels(wbIntlAuth(t, c))
	if len(got) != 1 || got[0] != "console-only" {
		t.Fatalf("names = %v, want the console answer", got)
	}
	// Order matters: /v2 is the priority path and must be tried first.
	first, second := -1, -1
	for i, p := range wbPaths(rt) {
		if p == "/v2/enterprises/personal/models" && first < 0 {
			first = i
		}
		if p == "/console/enterprises/personal/models" && second < 0 {
			second = i
		}
	}
	if first < 0 || second < 0 || first > second {
		t.Fatalf("probe order = %v, want /v2 before /console", wbPaths(rt))
	}
}

func TestWorkbuddyGlobalModelsUnionsBothUserAgents(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case v3ConfigPath:
			switch req.Header.Get("User-Agent") {
			case codeBuddyIDEUA:
				return jsonResponse(200, v3ConfigBody("ide-alpha", "shared-one")), nil
			case codeBuddyCLIUA:
				return jsonResponse(200, v3ConfigBody("cli-beta", "shared-one")), nil
			}
			return jsonResponse(200, v3ConfigBody("unknown-ua")), nil
		case "/v2/enterprises/personal/models", "/console/enterprises/personal/models":
			return jsonResponse(http.StatusInternalServerError, `{"msg":"down"}`), nil
		}
		return jsonResponse(http.StatusNotFound, `{}`), nil
	}}
	c, _ := panelClient(t, rt, intlAccountFiles())

	got := c.up.FetchGlobalModels(wbIntlAuth(t, c))
	if join := strings.Join(got, ","); join != "ide-alpha,shared-one,cli-beta" {
		t.Fatalf("union = %q, want the IDE answer first then the CLI-only ids", join)
	}

	uas := map[string]int{}
	for _, req := range rt.calls {
		if req.URL.Path == v3ConfigPath {
			uas[req.Header.Get("User-Agent")]++
		}
	}
	if uas[codeBuddyIDEUA] != 1 || uas[codeBuddyCLIUA] != 1 {
		t.Fatalf("v3/config UAs = %v, want one IDE probe and one CLI probe", uas)
	}
}

func TestWorkbuddyGlobalModelsCachesSuccessAndHonoursTheCooldown(t *testing.T) {
	t.Run("a fresh catalogue is reused", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == v3ConfigPath {
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			}
			return jsonResponse(200, globalEnterpriseBody("cached-1")), nil
		}}
		c, _ := panelClient(t, rt, intlAccountFiles())
		a := wbIntlAuth(t, c)

		first := c.up.FetchGlobalModels(a)
		after := len(rt.calls)
		second := c.up.FetchGlobalModels(a)

		if len(first) != 1 || len(second) != 1 || second[0] != "cached-1" {
			t.Fatalf("first = %v, second = %v, want the same catalogue", first, second)
		}
		if len(rt.calls) != after {
			t.Fatalf("the cached read made %d extra requests", len(rt.calls)-after)
		}
	})

	t.Run("a failed probe is not retried inside the cooldown", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusInternalServerError, `{"msg":"down"}`), nil
		}}
		c, _ := panelClient(t, rt, intlAccountFiles())
		a := wbIntlAuth(t, c)

		if got := c.up.FetchGlobalModels(a); got != nil {
			t.Fatalf("a failed probe returned %v, want nil", got)
		}
		first := len(rt.calls)
		if first == 0 {
			t.Fatal("the failed probe made no request at all")
		}
		if got := c.up.FetchGlobalModels(a); got != nil {
			t.Fatalf("second call returned %v, want nil", got)
		}
		if len(rt.calls) != first {
			t.Fatalf("the cooldown allowed %d more requests", len(rt.calls)-first)
		}
	})
}

func TestWorkbuddyGlobalModelsStoresTheRealmEfforts(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == v3ConfigPath {
			return jsonResponse(http.StatusInternalServerError, `{}`), nil
		}
		return jsonResponse(200, globalEnterpriseBody("effort-1")), nil
	}}
	c, _ := panelClient(t, rt, intlAccountFiles())

	infos := c.up.FetchGlobalModelInfos(wbIntlAuth(t, c))
	if len(infos) != 1 || infos[0].ID != "effort-1" {
		t.Fatalf("infos = %+v, want the enterprise metadata", infos)
	}
	if infos[0].ContextWindow != 200000 || infos[0].MaxTokens != 8192 {
		t.Fatalf("infos[0] = %+v, want the parsed windows", infos[0])
	}
	efforts, defaults := c.up.effortsSnapshot(realmGlobal), c.up.defaultEffortsSnapshot(realmGlobal)
	if len(efforts["effort-1"]) != 2 {
		t.Fatalf("global efforts = %v, want the two buckets", efforts)
	}
	if defaults["effort-1"] != "high" {
		t.Fatalf("global defaults = %v, want high", defaults)
	}
}

func TestWorkbuddyParseGlobalModelNames(t *testing.T) {
	t.Run("full entries", func(t *testing.T) {
		names, infos, err := parseGlobalModelNames([]byte(globalEnterpriseBody("a-1", "b-2")))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if strings.Join(names, ",") != "a-1,b-2" {
			t.Fatalf("names = %v, want the fixture order", names)
		}
		if len(infos) != 2 || infos[0].ContextWindow != 200000 {
			t.Fatalf("infos = %+v, want the metadata", infos)
		}
	})

	t.Run("a bare id list carries no metadata", func(t *testing.T) {
		names, infos, err := parseGlobalModelNames([]byte(`{"code":0,"data":["x-1","y-2"]}`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if strings.Join(names, ",") != "x-1,y-2" {
			t.Fatalf("names = %v", names)
		}
		if infos != nil {
			t.Fatalf("infos = %+v, want nil for a bare id list", infos)
		}
	})

	t.Run("a nested wrapper is unwrapped", func(t *testing.T) {
		names, _, err := parseGlobalModelNames([]byte(`{"code":0,"data":{"list":["n-1"]}}`))
		if err != nil || len(names) != 1 || names[0] != "n-1" {
			t.Fatalf("names = %v, err = %v", names, err)
		}
	})

	t.Run("a Name-only entry still gets an id", func(t *testing.T) {
		// The strict shape's own id fallback reaches `name` last, and its windows
		// live under maxInputTokens/maxOutputTokens with a nested reasoning block.
		body := `{"code":0,"data":{"models":[{"name":"named-1","maxInputTokens":64000,
		        "maxOutputTokens":4096,"reasoning":{"supportedEfforts":["medium"],"defaultEffort":"medium"}}]}}`
		names, infos, err := parseGlobalModelNames([]byte(body))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(names) != 1 || names[0] != "named-1" {
			t.Fatalf("names = %v", names)
		}
		if len(infos) != 1 || infos[0].ContextWindow != 64000 || infos[0].MaxTokens != 4096 ||
			infos[0].DefaultEffort != "medium" || len(infos[0].Efforts) != 1 {
			t.Fatalf("infos = %+v, want the strict fields", infos)
		}
	})

	t.Run("a payload the strict shape cannot decode falls back to the permissive one", func(t *testing.T) {
		// `tags` is a []string in the strict shape, so a scalar there makes the
		// whole strict decode fail; the permissive shape has no tags field at all
		// and is the only reason this catalogue still yields a model.
		body := `{"code":0,"data":{"models":[{"id":"loose-2","tags":"none",
		        "contextWindow":64000,"maxOutputTokens":4096,"supportedEfforts":["medium"],"effort":"medium"}]}}`
		names, infos, err := parseGlobalModelNames([]byte(body))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(names) != 1 || names[0] != "loose-2" {
			t.Fatalf("names = %v, want the permissive pass to answer", names)
		}
		if len(infos) != 1 || infos[0].ContextWindow != 64000 || infos[0].MaxTokens != 4096 ||
			infos[0].DefaultEffort != "medium" || len(infos[0].Efforts) != 1 {
			t.Fatalf("infos = %+v, want the camelCase fields", infos)
		}
	})

	t.Run("disabled entries are dropped", func(t *testing.T) {
		body := `{"code":0,"data":{"models":[{"id":"off-1","disabled":true},{"id":"on-2"}]}}`
		names, _, err := parseGlobalModelNames([]byte(body))
		if err != nil || len(names) != 1 || names[0] != "on-2" {
			t.Fatalf("names = %v, err = %v, want only the enabled entry", names, err)
		}
	})

	t.Run("failures are named", func(t *testing.T) {
		for _, tc := range []struct {
			body string
			want string
		}{
			{`{"code":7,"msg":"nope"}`, "global models code=7"},
			{`{"models":[]}`, "global models empty list"},
			{`{"code":0}`, "global models: no list found"},
			{``, "global models: no list found"},
		} {
			_, _, err := parseGlobalModelNames([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("body %q gave %v, want %q", tc.body, err, tc.want)
			}
		}
	})
}

func TestWorkbuddyMergeGlobalCatalog(t *testing.T) {
	primary := []ModelInfo{{ID: "a", ContextWindow: 100}, {ID: "b", ContextWindow: 200}}
	secondary := []ModelInfo{{ID: "b", ContextWindow: 999}, {ID: "c", ContextWindow: 300}}

	names, infos := mergeGlobalCatalog([]string{"a", "b"}, primary, []string{"b", "c"}, secondary)
	if strings.Join(names, ",") != "a,b,c" {
		t.Fatalf("names = %v, want the primary order then the new ids", names)
	}
	if len(infos) != 3 {
		t.Fatalf("infos = %+v, want three entries", infos)
	}
	if infos[1].ID != "b" || infos[1].ContextWindow != 200 {
		t.Fatalf("infos[1] = %+v, want the primary entry to win", infos[1])
	}

	names, infos = mergeGlobalCatalog([]string{"a"}, nil, []string{"b"}, secondary)
	if strings.Join(names, ",") != "a,b" {
		t.Fatalf("names = %v", names)
	}
	if infos != nil {
		t.Fatalf("infos = %+v, want nil when the primary had no metadata", infos)
	}

	names, _ = mergeGlobalCatalog([]string{"a", "", "a"}, nil, nil, nil)
	if len(names) != 1 || names[0] != "a" {
		t.Fatalf("names = %v, want empty ids and duplicates removed", names)
	}
}

func TestWorkbuddyMergeEffortMaps(t *testing.T) {
	primary := map[string][]string{"a": {"high"}, "shared": {"low"}}
	secondary := map[string][]string{"shared": {"medium"}, "b": {"high"}}

	got := mergeEffortBuckets(primary, secondary)
	if strings.Join(got["shared"], ",") != "low" {
		t.Fatalf("shared = %v, want the primary to win", got["shared"])
	}
	if len(got["b"]) != 1 || got["b"][0] != "high" {
		t.Fatalf("b = %v, want the secondary to fill it", got["b"])
	}
	if mergeEffortBuckets(nil, nil) != nil {
		t.Fatal("an empty union must be nil")
	}

	defPrimary := map[string]string{"a": "high", "shared": "low"}
	defSecondary := map[string]string{"shared": "medium", "b": "high"}
	defs := mergeEffortDefaults(defPrimary, defSecondary)
	if defs["shared"] != "low" || defs["b"] != "high" || defs["a"] != "high" {
		t.Fatalf("defaults = %v, want the primary to win and the secondary to fill", defs)
	}
	if mergeEffortDefaults(nil, nil) != nil {
		t.Fatal("an empty default union must be nil")
	}
}

func TestWorkbuddyDedupeAndCopyNames(t *testing.T) {
	got := dedupeNames([]string{"b", "", "a", "b", "c", "a"})
	if strings.Join(got, ",") != "b,a,c" {
		t.Fatalf("dedupeNames = %v, want order-preserving dedupe", got)
	}

	src := []string{"keep-1"}
	cp := copyNames(src)
	cp[0] = "mutated"
	if src[0] != "keep-1" {
		t.Fatalf("copyNames handed out the cache itself: %v", src)
	}
	if copyNames(nil) != nil {
		t.Fatal("copyNames(nil) must stay nil")
	}
}

func TestWorkbuddyGlobalModelsAnswersAnEmptyCatalogue(t *testing.T) {
	// A 200 with an empty list is a normal answer: it must not be cached as a
	// success, and it must not panic.
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == v3ConfigPath {
			return jsonResponse(http.StatusInternalServerError, `{}`), nil
		}
		return jsonResponse(200, `{"code":0,"data":{"models":[]}}`), nil
	}}
	c, _ := panelClient(t, rt, intlAccountFiles())

	if got := c.up.FetchGlobalModels(wbIntlAuth(t, c)); got != nil {
		t.Fatalf("names = %v, want nil for an empty catalogue", got)
	}
}

var _ = context.Background
