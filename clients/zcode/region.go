package zcode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// regionConfigTTL matches the upstream's own client-config cache window.
const regionConfigTTL = 10 * time.Minute

// regionInfo is the Aliyun traceless-verification configuration.
type regionInfo struct {
	Region  string
	SceneID string
	Prefix  string
	// Enabled mirrors the vendor's own captcha switch.  It is only meaningful
	// once the config endpoint has answered: a fetch that failed and a vendor
	// that turned its captcha off both leave this false, so it is reported to a
	// human rather than used to decide anything.  See CaptchaScene in
	// clients/zcode/captcha.go, which keys its decision on SceneID instead.
	Enabled bool
	// Known reports whether the vendor actually answered.  Without it a caller
	// cannot tell "captcha disabled" from "we could not ask".
	Known bool
}

// regionFor resolves the captcha region at runtime.  Precedence is
// config override > value fetched from the unauthenticated client-config
// endpoint.  Nothing is hardcoded: the two field reports disagree ("cn" vs
// "sgp") exactly because the upstream flips this value.
//
// An explicit override short-circuits: it exists so a deployment can pin the
// region WITHOUT talking to the endpoint, and the solver path (which is the
// only other caller) needs nothing else.  Callers that need the whole scene --
// scene id, prefix, the vendor's enabled flag -- ask sceneFor instead, which
// consults the endpoint either way.
func (p *pool) regionFor(ctx context.Context) regionInfo {
	if r := strings.TrimSpace(p.cfg.CaptchaRegion); r != "" {
		return regionInfo{Region: r}
	}
	return p.sceneFor(ctx)
}

// sceneFor resolves the whole captcha scene, always through the vendor's
// client-config endpoint (cached for regionConfigTTL).
//
// It is separate from regionFor because it deliberately does NOT let the region
// override suppress the fetch: the panel cannot run the vendor's captcha widget
// without a scene id, and a scene id can only come from here.  The override
// still wins for the REGION, because that is the value deployments disagree
// about.
func (p *pool) sceneFor(ctx context.Context) regionInfo {
	override := strings.TrimSpace(p.cfg.CaptchaRegion)

	p.reg.mu.Lock()
	defer p.reg.mu.Unlock()
	if !p.reg.loaded || time.Since(p.reg.fetchedAt) >= regionConfigTTL {
		info := p.fetchRegion(ctx)
		p.reg.loaded = true
		p.reg.fetchedAt = time.Now()
		p.reg.region, p.reg.sceneID, p.reg.prefix = info.Region, info.SceneID, info.Prefix
		p.reg.enabled, p.reg.known = info.Enabled, info.Known
		if info.Known && info.Region == "" {
			p.log("zcode: client/configs did not yield a captcha region; the JWT channel will fail until one is configured")
		}
	}

	info := regionInfo{
		Region:  p.reg.region,
		SceneID: p.reg.sceneID,
		Prefix:  p.reg.prefix,
		Enabled: p.reg.enabled,
		Known:   p.reg.known,
	}
	if override != "" {
		info.Region = override
	}
	return info
}

func (p *pool) fetchRegion(ctx context.Context) regionInfo {
	client := p.httpClient()
	if client == nil {
		return regionInfo{}
	}
	// The vendor validates this query string strictly: platform=win32 is
	// answered with HTTP 400 {"code":3001,"msg":"parameter error"}, which
	// silently cost this module its scene id and its region.  Send the same
	// identity the rest of the client sends, and never a hard-coded literal.
	q := url.Values{}
	q.Set("app_version", p.appVersion())
	if platform := strings.TrimSpace(p.cfg.Identity.Platform); platform != "" {
		q.Set("platform", platform)
	}
	u := configHost + "/api/v1/client/configs?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return regionInfo{}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", p.userAgent())

	resp, err := client.Do(req)
	if err != nil {
		p.log("zcode: captcha config fetch failed: %v", err)
		return regionInfo{}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		p.log("zcode: captcha config fetch returned HTTP %d", resp.StatusCode)
		return regionInfo{}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return regionInfo{}
	}
	var doc struct {
		Data struct {
			Configs struct {
				Captcha struct {
					Enabled bool   `json:"enabled"`
					Prefix  string `json:"prefix"`
					Region  string `json:"region"`
					SceneID string `json:"sceneId"`
				} `json:"captcha"`
			} `json:"configs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return regionInfo{}
	}
	c := doc.Data.Configs.Captcha
	return regionInfo{
		Region:  strings.TrimSpace(c.Region),
		SceneID: strings.TrimSpace(c.SceneID),
		Prefix:  strings.TrimSpace(c.Prefix),
		Enabled: c.Enabled,
		Known:   true,
	}
}
