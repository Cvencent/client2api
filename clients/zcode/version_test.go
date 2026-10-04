package zcode

import (
	"context"
	"net/http"
	"testing"
)

// staleAppVersion is a build the vendor no longer offers the daily Start Plan
// to.  The live vendor answered 3.11.2/3.12/3.13 with an empty plan list and
// 3.14.4 with a claimable plan on 2026-10-03, which is the whole reason
// version.go exists.
const staleAppVersion = "3.11.2"

// manifestRoute answers the release manifest with one version line, the shape
// the official updater reads.
func manifestRoute(version string) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return newResponse(http.StatusOK, "text/yaml", "version: "+version+"\nreleaseName: Release v"+version+"\n"), nil
	}
}

// previewByVersion answers an empty plan list to a stale build and a claimable
// plan to a current one, and records which version each request claimed to be.
func previewByVersion(seen *[]string) func(*http.Request) (*http.Response, error) {
	return func(r *http.Request) (*http.Response, error) {
		v := r.URL.Query().Get("app_version")
		if seen != nil {
			*seen = append(*seen, v)
		}
		if v == "3.14.4" {
			return jsonResponse(http.StatusOK, `{"code":0,"data":{"plans":[`+
				`{"plan_id":"zcode-v3-start-plan-trust-1003","name":"ZCode Trust Build","priority":110}`+
				`]}}`), nil
		}
		return jsonResponse(http.StatusOK, `{"code":0,"data":{"plans":[]}}`), nil
	}
}

// TestClaimPreviewAdoptsTheAdvertisedVersion is the regression test for the bug
// this file was written for: a pinned app version silently turns the daily
// promotion into "nothing is claimable right now".  The preview must notice the
// empty answer, adopt the version the release manifest advertises, and re-ask.
func TestClaimPreviewAdoptsTheAdvertisedVersion(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	var seen []string
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath:     previewByVersion(&seen),
		releaseManifestPath: manifestRoute("3.14.4"),
	}))
	c.cfg.Identity.AppVersion = staleAppVersion
	id := addJWTAccount(t, c)

	plans, err := c.claimPreview(context.Background(), c.pool.find(id))
	if err != nil {
		t.Fatalf("claimPreview: %v", err)
	}
	if len(plans) != 1 || plans[0].id() != "zcode-v3-start-plan-trust-1003" {
		t.Fatalf("plans = %+v, want the plan the current build is offered", plans)
	}
	if got := c.appVersion(); got != "3.14.4" {
		t.Errorf("resolved app version = %q, want the advertised 3.14.4", got)
	}
	if len(seen) != 2 || seen[0] != staleAppVersion || seen[1] != "3.14.4" {
		t.Errorf("preview versions = %v, want [%s 3.14.4]", seen, staleAppVersion)
	}
}

// TestClaimPreviewDoesNotReAskAnUpToDateBuild pins the other half: when the
// manifest has nothing newer, an empty plan list is the vendor's real answer
// and the preview is sent exactly once.
func TestClaimPreviewDoesNotReAskAnUpToDateBuild(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	var seen []string
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath:     previewByVersion(&seen),
		releaseManifestPath: manifestRoute(staleAppVersion),
	}))
	c.cfg.Identity.AppVersion = staleAppVersion
	id := addJWTAccount(t, c)

	plans, err := c.claimPreview(context.Background(), c.pool.find(id))
	if err != nil {
		t.Fatalf("claimPreview: %v", err)
	}
	if len(plans) != 0 {
		t.Fatalf("plans = %+v, want none", plans)
	}
	if len(seen) != 1 || seen[0] != staleAppVersion {
		t.Errorf("preview versions = %v, want exactly [%s]", seen, staleAppVersion)
	}
	if got := c.appVersion(); got != staleAppVersion {
		t.Errorf("resolved app version = %q, want the configured %q", got, staleAppVersion)
	}
}

// TestResolvedVersionDrivesTheIdentityHeaders proves the adopted version is
// what goes on the wire, not just what an accessor returns.
func TestResolvedVersionDrivesTheIdentityHeaders(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		releaseManifestPath: manifestRoute("3.15.0"),
	}))
	c.cfg.Identity.AppVersion = staleAppVersion
	// Prime the cache the way a claim would.
	if v := c.pool.refreshAppVersion(context.Background()); v != "3.15.0" {
		t.Fatalf("refreshAppVersion = %q, want 3.15.0", v)
	}

	h := http.Header{}
	c.applyIdentityHeaders(h)
	if got := h.Get("X-ZCode-App-Version"); got != "3.15.0" {
		t.Errorf("X-ZCode-App-Version = %q, want 3.15.0", got)
	}
	if got := h.Get("User-Agent"); got != "ZCode/3.15.0" {
		t.Errorf("User-Agent = %q, want ZCode/3.15.0", got)
	}
}

func TestVersionGreater(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"3.14.4", "3.11.2", true},
		{"3.14.4", "3.14.4", false},
		{"3.14.4", "3.14.10", false},
		{"3.15", "3.14.4", true},
		{"4.0.0", "3.99.99", true},
		{"3.14.4", "", true},
		{"", "3.14.4", false},
		{"nonsense", "3.14.4", false},
		{"3.14.4-beta.1", "3.14.3", true},
	}
	for _, tc := range cases {
		if got := versionGreater(tc.a, tc.b); got != tc.want {
			t.Errorf("versionGreater(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestManifestVersion(t *testing.T) {
	if got := manifestVersion([]byte("version: 3.14.4\nreleaseName: Release v3.14.4\n")); got != "3.14.4" {
		t.Errorf("manifestVersion = %q, want 3.14.4", got)
	}
	if got := manifestVersion([]byte("releaseName: no version here\n")); got != "" {
		t.Errorf("manifestVersion = %q, want empty", got)
	}
	if got := manifestVersion([]byte("version: \"3.14.4\"\n")); got != "3.14.4" {
		t.Errorf("quoted manifestVersion = %q, want 3.14.4", got)
	}
}

func TestReleasePlatformFor(t *testing.T) {
	cases := map[string]string{
		"win32-x64":    "windows-x86_64",
		"win32-arm64":  "windows-aarch64",
		"darwin-x64":   "darwin-x86_64",
		"darwin-arm64": "darwin-aarch64",
		"linux-x64":    "linux-x86_64",
		"":             "windows-x86_64",
	}
	for in, want := range cases {
		if got := releasePlatformFor(in); got != want {
			t.Errorf("releasePlatformFor(%q) = %q, want %q", in, got, want)
		}
	}
}
