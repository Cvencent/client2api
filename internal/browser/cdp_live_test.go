package browser

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveLaunch drives the public Launch API end to end: start a browser,
// attach to its page, evaluate script, navigate, fill and click.  It needs a
// real browser, so it is skipped unless C2A_LIVE_CDP is set.
func TestLiveLaunch(t *testing.T) {
	if os.Getenv("C2A_LIVE_CDP") == "" {
		t.Skip("set C2A_LIVE_CDP=1 to run the live browser test")
	}
	root, _ := filepath.Abs(filepath.Join("run", "browser"))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := Launch(ctx, LaunchOpts{
		ProfileRoot: root,
		Headless:    true,
		StartURL:    "about:blank",
		Timeout:     45 * time.Second,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer b.Close()
	t.Logf("driving %s", b.ExecPath())

	page := b.Page()

	got, err := page.EvalString(ctx, `1+1`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	t.Logf("1+1 -> %q (non-string reads as empty, which is fine)", got)

	raw, err := page.Eval(ctx, `1+1`)
	if err != nil {
		t.Fatalf("eval number: %v", err)
	}
	if string(raw) != "2" {
		t.Fatalf("1+1 = %s, want 2", raw)
	}

	if err := page.Navigate(ctx, "data:text/html,<title>probe</title><input id=x><button id=b onclick=\"this.textContent='clicked'\">go</button>"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	title, err := page.EvalString(ctx, `document.title`)
	if err != nil {
		t.Fatalf("title: %v", err)
	}
	if title != "probe" {
		t.Fatalf("title = %q, want probe", title)
	}

	if ok, err := page.Fill(ctx, "#x", "hello"); err != nil || !ok {
		t.Fatalf("fill: ok=%v err=%v", ok, err)
	}
	if v, err := page.EvalString(ctx, `document.getElementById('x').value`); err != nil || v != "hello" {
		t.Fatalf("input value = %q err=%v, want hello", v, err)
	}
	if ok, err := page.Click(ctx, "#b"); err != nil || !ok {
		t.Fatalf("click: ok=%v err=%v", ok, err)
	}
	if v, err := page.EvalString(ctx, `document.getElementById('b').textContent`); err != nil || v != "clicked" {
		t.Fatalf("button text = %q err=%v, want clicked", v, err)
	}

	if png, err := page.Screenshot(ctx); err != nil || len(png) < 1000 {
		t.Fatalf("screenshot: %d bytes err=%v", len(png), err)
	}
}
