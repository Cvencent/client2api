package doubao

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"client2api/internal/browser"
)

// browserlogin.go wraps the shared CDP driver for the web login.  Everything
// vendor-specific (which URL to open, which cookies prove a session) stays
// here; the driver only knows how to start a Chromium-family browser and read
// its cookies.

// loginBrowser is the seam the tests replace: a launched browser that can
// report its cookies and be closed.  In production it is a *browser.Browser.
type loginBrowser interface {
	Cookies(ctx context.Context, urls []string) ([]browser.Cookie, error)
	Close()
}

// launchBrowser is the injectable launcher.  Tests override it; production uses
// the shared driver.
var launchBrowser = func(ctx context.Context, opts browser.LaunchOpts) (loginBrowser, error) {
	b, err := browser.Launch(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &cdpBrowser{b: b}, nil
}

// cdpBrowser adapts *browser.Browser to loginBrowser.
type cdpBrowser struct{ b *browser.Browser }

func (c *cdpBrowser) Cookies(ctx context.Context, urls []string) ([]browser.Cookie, error) {
	return c.b.Page().Cookies(ctx, urls)
}

func (c *cdpBrowser) Close() {
	if c.b != nil {
		c.b.Close()
	}
}

// launchLoginBrowser starts a visible browser on the Doubao sign-in page.  The
// profile is throwaway, so a login never touches the operator's own browser
// profile or sessions.
func launchLoginBrowser(ctx context.Context, cfg config, pageURL, profileRoot string) (loginBrowser, error) {
	lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	opts := browser.LaunchOpts{
		ExecPath:    cfg.browserPath(),
		ProfileRoot: profileRoot,
		Headless:    cfg.browserHeadless(),
		StartURL:    pageURL,
		Timeout:     browser.DefaultLaunchTimeout,
	}
	return launchBrowser(lctx, opts)
}

// captureSessionCookies reads the Doubao cookies out of the controlled browser
// and renders them as a request Cookie header.  Only cookies for the Doubao
// domain are kept: a shared jar carries unrelated tracker cookies the vendor
// never expects in the session.
func captureSessionCookies(ctx context.Context, br loginBrowser, origin string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cookies, err := br.Cookies(rctx, []string{origin})
	if err != nil {
		return "", err
	}
	pairs := make([][2]string, 0, len(cookies))
	for _, c := range cookies {
		if strings.TrimSpace(c.Name) == "" || c.Value == "" {
			continue
		}
		if !strings.Contains(c.Domain, "doubao.com") {
			continue
		}
		pairs = append(pairs, [2]string{c.Name, c.Value})
	}
	return cookieHeaderFromValues(pairs), nil
}

// loginProfileRoot is where the throwaway profile is created.  It lives under
// Deps.DataDir when one was supplied, so nothing outside the module's own
// directory holds a credential; otherwise the shared driver's default applies.
func (c *Client) loginProfileRoot() string {
	if c.deps.DataDir == "" {
		return ""
	}
	return filepath.Join(c.deps.DataDir, "browser")
}
