//go:build ignore

// Command uilook renders a panel view, or the account-model dialog, to a PNG so
// a UI change can be reviewed without a live gateway.  It boots the real
// internal/panel handler against a fake registry in a headless browser and
// screenshots exactly what the panel server serves.
//
// It is a developer harness, not a shipped binary, so it carries the
// `//go:build ignore` constraint below.  That is what keeps it out of
// `go vet ./...`, `go build ./...` and `go test ./...` while `go run` still
// compiles and runs it on demand.  See tools/README.md.
//
// Usage:
//
//	go run ./tools/uilook/main.go -view accounts -out uilook.png
//	go run ./tools/uilook/main.go -editable -theme light -width 1440 -height 960
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"client2api/internal/browser"
	"client2api/internal/core"
	"client2api/internal/modelpolicy"
	"client2api/internal/panel"
)

type fake struct {
	name string
}

func (f *fake) Name() string { return f.name }
func (f *fake) Models(context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "deepseek-v4.1-flash"}, {ID: "deepseek-v4.1-pro"}, {ID: "glm-4.7"}, {ID: "qwen3-max"}}, nil
}
func (f *fake) Chat(context.Context, *core.ChatRequest) (core.Stream, error) {
	return nil, core.ErrUnsupported
}
func (f *fake) Status(context.Context) core.Status {
	return core.Status{Name: f.name, Ready: true, UpdatedAt: time.Now(), Models: []string{"deepseek-v4.1-flash"},
		Accounts: []core.AccountStatus{{
			ID: "acct-a", Label: "demo@example.com", Enabled: true, State: "ready",
			Extra: map[string]any{"realm": "global", "model_cooldowns": []map[string]any{
				{"model": "deepseek-v4.1-flash", "kind": "rate_limit", "reason": "6004 model rate limited", "until": time.Now().Add(90 * time.Minute).UTC().Format(time.RFC3339)},
			}},
		}},
	}
}

type mgr struct{ *fake }

func (m *mgr) AccountFields(context.Context) []core.FieldSpec { return nil }
func (m *mgr) Accounts(context.Context) ([]core.AccountRecord, error) {
	return []core.AccountRecord{{
		ID: "acct-a", Label: "demo@example.com", Enabled: true, State: "ready",
		Fields: map[string]any{"realm": "global", "disabled_models": []string{"qwen3-max"}},
	}}, nil
}
func (m *mgr) AddAccount(context.Context, core.AccountSpec) (core.AccountRecord, error) {
	return core.AccountRecord{}, nil
}
func (m *mgr) RemoveAccount(context.Context, string) error           { return nil }
func (m *mgr) SetAccountEnabled(context.Context, string, bool) error { return nil }
func (m *mgr) TestAccount(context.Context, string) (core.TestResult, error) {
	return core.TestResult{}, nil
}
func (m *mgr) RefreshAccount(context.Context, string) ([]core.RefreshResult, error) { return nil, nil }

func main() {
	editable := flag.Bool("editable", false, "render the editable dialog (shared model policy store)")
	view := flag.String("view", "", "panel view to render (accounts, chat, usage, alerts, credits, packages, taskscenter, models, platforms, config, logs)")
	theme := flag.String("theme", "dark", "theme to force (dark, light, system)")
	width := flag.Int("width", 1440, "browser viewport width")
	height := flag.Int("height", 960, "browser viewport height")
	out := flag.String("out", "uilook.png", "screenshot path")
	flag.Parse()

	reg := core.NewRegistry()
	reg.Add(&mgr{&fake{name: "workbuddy"}})

	var store *modelpolicy.Store
	if *editable {
		store, _ = modelpolicy.Open("")
	}
	h := panel.New(panel.Options{Registry: reg, Version: "look", Started: time.Now(), ModelPolicies: store})
	mux := http.NewServeMux()
	mux.Handle("/", h)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go func() { _ = http.Serve(ln, mux) }()
	base := "http://" + ln.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	b, err := browser.Launch(ctx, browser.LaunchOpts{
		Headless:    true,
		ProfileRoot: filepath.Join(os.TempDir(), "uilook-profile"),
		StartURL:    base + "/panel/",
		Timeout:     25 * time.Second,
		ExtraArgs:   []string{fmt.Sprintf("--window-size=%d,%d", *width, *height)},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer b.Close()
	page := b.Page()
	ready := `(() => { const h = document.documentElement; return !!(h && (h.getAttribute("data-c2a-ready") === "1" || h.getAttribute("data-c2a-boot-error") === "1")); })()`
	if ok, err := page.WaitFor(ctx, ready, 15*time.Second); err != nil || !ok {
		log.Fatalf("panel did not boot: ok=%v err=%v", ok, err)
	}
	if _, err := page.Eval(ctx, `(() => { localStorage.setItem("c2a.key",""); })()`); err != nil {
		log.Fatal(err)
	}
	if *theme != "system" {
		if _, err := page.Eval(ctx, `(() => { document.documentElement.setAttribute("data-theme", `+strconv.Quote(*theme)+`); })()`); err != nil {
			log.Fatal(err)
		}
	}
	if *view != "" {
		got, err := page.EvalString(ctx, `(() => { go(`+strconv.Quote(*view)+`); return VIEW; })()`)
		if err != nil {
			log.Fatalf("select view: %v", err)
		}
		fmt.Println("view:", got)
		time.Sleep(900 * time.Millisecond)
		png, err := page.Screenshot(ctx)
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(*out, png, 0o600); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote", *out)
		return
	}
	// Drive the page into the state that shows the model dialog.
	script := `
	(() => {
	  if (typeof openModelsDialog !== "function") return "no openModelsDialog";
	  const n = "workbuddy";
	  ACCTS[n] = ACCTS[n] || {};
	  ACCTS[n].capabilities = Object.assign({}, ACCTS[n].capabilities || {}, {manage:true, model_policy: %s, model_policy_store: %s});
	  SNAP.clients = SNAP.clients || [];
	  let c = SNAP.clients.find(x => x.name === n);
	  if (!c) { c = {name:n}; SNAP.clients.push(c); }
	  c.models = ["deepseek-v4.1-flash","deepseek-v4.1-pro","glm-4.7","qwen3-max"];
	  c.capabilities = Object.assign({}, c.capabilities || {}, {manage:true, model_policy: %s, model_policy_store: %s});
	  MODEL_NAMES_READY[""] = true;
	  const a = {id:"acct-a", label:"demo@example.com", enabled:true, state:"ready",
	             fields:{realm:"global", disabled_models:["qwen3-max"]}};
	  POOL = {redis_mode:false, clients:{[n]:{pool:null, health:null, extra:{"acct-a":{model_cooldowns:[{model:"deepseek-v4.1-flash", kind:"rate_limit", until:new Date(Date.now()+90*60000).toISOString()},{model:"glm-4.7", kind:"unsupported"}]}}}}};
	  applyAccounts(n, {accounts:[a]});
	  openModelsDialog(n, ["acct-a"]);
	  return "opened:" + (!!document.querySelector(".mpark-acct"));
	})()`
	js := fmt.Sprintf(script, boolJS(*editable), boolJS(*editable), boolJS(*editable), boolJS(*editable))
	got, err := page.EvalString(ctx, js)
	if err != nil {
		log.Fatalf("drive: %v", err)
	}
	fmt.Println("drive result:", got)
	time.Sleep(400 * time.Millisecond)
	png, err := page.Screenshot(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, png, 0o600); err != nil {
		log.Fatal(err)
	}
	fmt.Println("wrote", *out)

}

func boolJS(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
