package zcode

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The vendor gates several things on the client version it is told: the daily
// "Start Plan" promotion is only offered to callers that claim to be a current
// build (3.14.4 answered with a claimable plan where 3.11.2/3.12/3.13 answered
// with an empty list), and the client-config document is parameterised by it.
// A pinned constant therefore rots the moment the vendor ships a minor release,
// and the failure is silent: the promotion simply stops appearing.
//
// This file keeps the claimed version current by reading the same release
// manifest the official client's updater reads.  It is deliberately lazy and
// cached.  Nothing fetches on the chat path; the manifest is only consulted
// when the vendor answers an empty plan document, which is exactly the signal a
// stale version produces, and the result is reused for an hour afterwards.
const (
	// appVersionTTL is how long an advertised version is trusted before the
	// manifest is read again.
	appVersionTTL = time.Hour

	// releaseManifestPath is the official updater's manifest route
	// (electron-updater's Provider feed).  It is unauthenticated.
	releaseManifestPath = "/api/v1/releases/electron/manifest"

	// releaseManifestMaxBytes bounds the manifest read.  The live document is
	// under 2 KB.
	releaseManifestMaxBytes = 1 << 16
)

// versionCache holds the last version the release manifest advertised.
type versionCache struct {
	mu        sync.Mutex
	latest    string
	fetchedAt time.Time
	loaded    bool
}

// appVersion is the version this client claims to the vendor.
func (c *Client) appVersion() string { return c.pool.appVersion() }

// appVersion is the version every vendor-facing request claims to be.  It is
// the configured value unless the release manifest has advertised a newer one,
// so an operator override can always pin an exact build (including a newer one
// the manifest does not know about yet).
func (p *pool) appVersion() string {
	configured := strings.TrimSpace(p.cfg.Identity.AppVersion)
	p.ver.mu.Lock()
	latest := p.ver.latest
	p.ver.mu.Unlock()
	if latest != "" && versionGreater(latest, configured) {
		return latest
	}
	return configured
}

// refreshAppVersion reads the release manifest (at most once per appVersionTTL)
// and returns the version it advertises, or "" when it could not be read.  A
// failure is cached too, so a vendor outage cannot turn every claim attempt
// into a manifest request.
func (p *pool) refreshAppVersion(ctx context.Context) string {
	p.ver.mu.Lock()
	if p.ver.loaded && time.Since(p.ver.fetchedAt) < appVersionTTL {
		latest := p.ver.latest
		p.ver.mu.Unlock()
		return latest
	}
	p.ver.mu.Unlock()

	latest := p.fetchLatestAppVersion(ctx)

	p.ver.mu.Lock()
	p.ver.loaded = true
	p.ver.fetchedAt = time.Now()
	if latest != "" {
		p.ver.latest = latest
	}
	out := p.ver.latest
	p.ver.mu.Unlock()
	return out
}

// fetchLatestAppVersion performs one manifest read.  It never fails loudly: a
// missing manifest means "keep the configured version", which is the state the
// module was in before this file existed.
func (p *pool) fetchLatestAppVersion(ctx context.Context) string {
	client := p.httpClient()
	if client == nil {
		return ""
	}

	q := url.Values{}
	q.Set("platform", releasePlatformFor(p.cfg.Identity.Platform))
	q.Set("channel", "1") // 1 = stable, 3 = preview (the updater's own mapping)
	u := configHost + releaseManifestPath + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/x-yaml,text/yaml,text/plain,*/*")
	req.Header.Set("User-Agent", p.userAgent())

	resp, err := client.Do(req)
	if err != nil {
		p.log("zcode: release manifest fetch failed: %v", err)
		return ""
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		p.log("zcode: release manifest returned HTTP %d", resp.StatusCode)
		return ""
	}
	return manifestVersion(readLimited(resp.Body, releaseManifestMaxBytes))
}

// releasePlatformFor maps the identity platform the rest of the module sends
// ("win32-x64") onto the updater's platform token ("windows-x86_64").
//
// The two spellings come from different upstream endpoints and are not
// interchangeable; the updater's mapping is the authority for this route.
func releasePlatformFor(platform string) string {
	p := strings.ToLower(strings.TrimSpace(platform))
	os := ""
	switch {
	case strings.HasPrefix(p, "win"):
		os = "windows"
	case strings.HasPrefix(p, "darwin"), strings.HasPrefix(p, "mac"):
		os = "darwin"
	case strings.HasPrefix(p, "linux"):
		os = "linux"
	}
	arch := ""
	switch {
	case strings.Contains(p, "arm64"), strings.Contains(p, "aarch64"):
		arch = "aarch64"
	case strings.Contains(p, "ia32"), strings.Contains(p, "x86-32"):
		arch = "x86"
	case strings.Contains(p, "64"):
		arch = "x86_64"
	}
	if os == "" {
		os = "windows"
	}
	if arch == "" {
		arch = "x86_64"
	}
	return os + "-" + arch
}

// manifestVersion extracts the top-level "version:" line from a release
// manifest.  The document is YAML, but the one field this module needs is a
// scalar on its own line, and the tree has no YAML dependency by design.
func manifestVersion(raw []byte) string {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "version:") {
			continue
		}
		v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "version:")), `"'`)
		if parseVersion(v) != nil {
			return v
		}
	}
	return ""
}

// versionGreater reports whether a is a strictly newer dotted version than b.
// Unparseable inputs are never "greater", so a malformed manifest value cannot
// displace a good configured version.
func versionGreater(a, b string) bool {
	av, bv := parseVersion(a), parseVersion(b)
	if av == nil {
		return false
	}
	if bv == nil {
		return true
	}
	for i := 0; i < 3; i++ {
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	return false
}

// parseVersion reads up to three leading dotted integers ("3.14.4", "3.15").
// A trailing pre-release tag is ignored, which is all the ordering this module
// needs: it only decides whether to adopt a version the vendor advertises.
func parseVersion(v string) []int {
	v = strings.TrimSpace(strings.Trim(v, `"'`))
	if v == "" {
		return nil
	}
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return nil
	}
	out := []int{0, 0, 0}
	for i := 0; i < len(parts) && i < 3; i++ {
		n := 0
		if parts[i] == "" {
			return nil
		}
		for _, r := range parts[i] {
			if r < '0' || r > '9' {
				return nil
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out
}
