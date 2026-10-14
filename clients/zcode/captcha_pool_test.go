package zcode

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestCaptchaPoolTakeConsumesTheOnlyParam(t *testing.T) {
	pool := newCaptchaPool(30 * time.Second)
	now := time.Unix(1000, 0)

	if pool.hasFresh(now) {
		t.Fatal("a new pool must be empty")
	}
	pool.put("  tok  ", " cn ", now)
	if !pool.hasFresh(now) {
		t.Fatal("hasFresh must not consume the pooled param")
	}

	got, ok := pool.take(now)
	if !ok || got.param != "tok" || got.region != "cn" {
		t.Fatalf("take = (%+v,%v), want the trimmed pooled param", got, ok)
	}
	if _, ok := pool.take(now); ok {
		t.Fatal("take must remove the param so it can never be reused")
	}

	pool.put("expired", "cn", now)
	if _, ok := pool.take(now.Add(30 * time.Second)); ok {
		t.Fatal("a param at its TTL deadline must not be handed out")
	}
}

func TestSolveCaptchaPoolPrefetchesAndConsumesOneShotParams(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false,"captcha_browser":true,"captcha_pool":true,"captcha_pool_ttl_seconds":30}`)
	ft := routeTransport(t, captchaRoutes(captchaConfigFixture, nil))
	c := env.client(t, ft)

	var mu sync.Mutex
	seq := 0
	c.mintBrowser = func(context.Context, regionInfo) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		seq++
		return fmt.Sprintf("browser-token-%d", seq), nil
	}

	first, _, err := c.solveCaptcha(context.Background())
	if err != nil {
		t.Fatalf("first solveCaptcha: %v", err)
	}
	if first != "browser-token-1" {
		t.Fatalf("first solveCaptcha = %q, want the synchronous mint", first)
	}
	waitForCaptchaPool(t, c)

	second, _, err := c.solveCaptcha(context.Background())
	if err != nil {
		t.Fatalf("second solveCaptcha: %v", err)
	}
	if second == first {
		t.Fatalf("second solveCaptcha reused %q; a taken param must be consumed", second)
	}

	third, _, err := c.solveCaptcha(context.Background())
	if err != nil {
		t.Fatalf("third solveCaptcha: %v", err)
	}
	if third == second || third == first {
		t.Fatalf("third solveCaptcha = %q; it reused an already-consumed param", third)
	}
}

func TestSolveCaptchaPoolPrefetchFailureDoesNotBreakTheNextRequest(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false,"captcha_browser":true,"captcha_pool":true,"captcha_pool_ttl_seconds":30}`)
	ft := routeTransport(t, captchaRoutes(captchaConfigFixture, nil))
	c := env.client(t, ft)

	prefetchFailed := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	c.mintBrowser = func(context.Context, regionInfo) (string, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 2 {
			close(prefetchFailed)
			return "", errors.New("prefetch failed")
		}
		return fmt.Sprintf("browser-token-%d", call), nil
	}

	if _, _, err := c.solveCaptcha(context.Background()); err != nil {
		t.Fatalf("first solveCaptcha: %v", err)
	}
	select {
	case <-prefetchFailed:
	case <-time.After(2 * time.Second):
		t.Fatal("the background prefetch did not run")
	}

	got, _, err := c.solveCaptcha(context.Background())
	if err != nil {
		t.Fatalf("solveCaptcha after a prefetch failure: %v", err)
	}
	if got == "" {
		t.Fatal("solveCaptcha returned an empty param")
	}
}

// A configured command is the headless path, and it must get the same
// prefetch treatment as the built-in browser.  Without this the pool is
// enabled in the config but every request still blocks on the command.
func TestSolveCaptchaPoolPrefetchesThroughTheCommandSolver(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false,"captcha_browser":false,"captcha_pool":true,"captcha_pool_ttl_seconds":30}`)
	ft := routeTransport(t, captchaRoutes(captchaConfigFixture, nil))
	c := env.client(t, ft)
	c.cfg.CaptchaRegion = "cn"
	installSolver(t, c, "command-token-1")

	first, _, err := c.solveCaptcha(context.Background())
	if err != nil {
		t.Fatalf("first solveCaptcha: %v", err)
	}
	if first != "command-token-1" {
		t.Fatalf("first solveCaptcha = %q, want the synchronous command mint", first)
	}
	waitForCaptchaPool(t, c)

	// The pooled slot was prefetched with the same command, so this request
	// must read it instead of minting synchronously.  The fixture command is
	// deterministic, which is exactly what makes this observable: waitForCaptchaPool
	// above proves the command path refilled the slot at all.
	second, _, err := c.solveCaptcha(context.Background())
	if err != nil {
		t.Fatalf("second solveCaptcha: %v", err)
	}
	if second != "command-token-1" {
		t.Fatalf("second solveCaptcha = %q, want the command solver's output", second)
	}
}

func waitForCaptchaPool(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if c.captchaPool.hasFresh(time.Now()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the captcha pool was not refilled")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
