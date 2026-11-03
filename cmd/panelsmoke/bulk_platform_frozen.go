package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"client2api/internal/browser"
)

// assertBulkTestPlatformFrozen reproduces issue #158 in the live page: the
// bulk "test all accounts" run must keep addressing the platform it started
// on, even after the operator switches views.
//
// The reported shape: the loop re-read the global CUR every round, so
// switching platforms mid-run sent the remaining probes to the NEW platform's
// /test with the OLD platform's account ids (a burst of 4xx), wrote results
// into the new platform's cache, and the closing toast reported the new
// platform's name with a mixed count.
//
// The probe works without a slow vendor: the per-account /test call is
// intercepted by a fetch stub that answers instantly but asynchronously, and
// the platform switch happens after the first round has fired.  The run is
// then measured by which platform the /test calls addressed.
func assertBulkTestPlatformFrozen(ctx context.Context, page *browser.Page) error {
	if page == nil {
		return nil
	}
	if _, err := page.Eval(ctx, `(() => { if (typeof go === "function") go("accounts"); return true; })()`); err != nil {
		return fmt.Errorf("open the accounts view: %w", err)
	}
	// Seed two platforms with accounts: the smoke instance has none, and the
	// frozen-platform property needs a second platform to switch to (the
	// seeding follows assertAccountsKeyboard's shape).
	if _, err := page.Eval(ctx, `(() => {
	  const caps = { manage: true, actions: [], test: true };
      ACCTS["qa-a"] = { capabilities: caps, accounts: [
        { id: "a1", label: "a1", state: "ready", enabled: true, fields: {}, note: "", priorities: {} },
        { id: "a2", label: "a2", state: "ready", enabled: true, fields: {}, note: "", priorities: {} },
        { id: "a3", label: "a3", state: "ready", enabled: true, fields: {}, note: "", priorities: {} }] };
      ACCTS["qa-b"] = { capabilities: caps, accounts: [
        { id: "b1", label: "b1", state: "ready", enabled: true, fields: {}, note: "", priorities: {} }] };
      if (typeof renderAccounts === "function") renderAccounts();
      return true;
	})()`); err != nil {
		return fmt.Errorf("seed the two platforms: %w", err)
	}
	// Run the bulk test on the first seeded platform while logging every
	// /test URL the page issues, and switch platforms mid-run.
	report, err := page.EvalString(ctx, `(() => {
	  const problems = [];
	  // Pin the seeded platforms explicitly: ACCTS also carries the real
	  // registered platforms (some with zero enabled accounts, where bulk()
	  // returns before issuing anything).
	  const target = "qa-a";
	  const other = "qa-b";
	  if (!ACCTS[target] || !ACCTS[other]) return "seeded platforms missing";
	  if (typeof CUR === "undefined") return "no CUR global";
	  if (typeof bulk !== "function") return "no bulk function";

	  // Stub every /accounts/<id>/test call: answer 200 with result.ok=true,
	  // but asynchronously (a real round trip the switch can interleave with),
	  // and record which platform each call addressed.
	  window.__c2a158 = { calls: [], real: window.fetch };
	  window.fetch = function(input, init) {
	    const url = typeof input === "string" ? input : (input && input.url) || "";
	    const cIdx = String(url).indexOf("/clients/");
	    const tIdx = String(url).indexOf("/test");
	    if (cIdx >= 0 && tIdx > cIdx) {
	      const rest = String(url).substring(cIdx + "/clients/".length, tIdx);
	      const plat = rest.split("/accounts/")[0];
	      window.__c2a158.calls.push(plat);
	      const body = JSON.stringify({accounts: [], result: {ok: true, model: "probe"}});
	      return new Promise(res => setTimeout(() => res(new Response(body,
	        {status: 200, headers: {"Content-Type": "application/json"}})), 60));
	    }
	    return window.__c2a158.real.apply(this, arguments);
	  };

	  // Start the bulk test on the target platform.
	  CUR = target;
	  if (typeof renderAccounts === "function") renderAccounts();
      const list = ((ACCTS[target] || {}).accounts || []).filter(a => a.enabled);
      if (!list.length) return "";
	  bulk("test");

	  // Switch away mid-run: the first stubbed answer resolves ~60ms later, so
	  // by now at least one round is in flight.
	  return new Promise(done => setTimeout(() => {
	    CUR = other;
	    window.__c2a158.dbgMid = CUR + "|" + window.__c2a158.calls.join(",");
	    if (typeof renderAccounts === "function") renderAccounts();
	    setTimeout(() => done("switched to " + CUR + "; calls=" + JSON.stringify(window.__c2a158.calls)), 800);
	  }, 120));
	})()`)
	if err != nil {
		return fmt.Errorf("run the frozen-platform probe: %w", err)
	}
	// Let the async bulk run finish.
	time.Sleep(2500 * time.Millisecond)
	dbg, _ := page.EvalString(ctx, `(() => JSON.stringify({mid: window.__c2a158.dbgMid, calls: window.__c2a158.calls}))()`)
	fmt.Printf("panelsmoke: [dbg 158] %s\n", dbg)
	// Read the calls back (the promise above only carried the mid-run state).
	calls, err := page.EvalString(ctx, `(() => JSON.stringify((window.__c2a158 || {calls: []}).calls))()`)
	if err != nil {
		return fmt.Errorf("read the probe log: %w", err)
	}
	// Restore the real fetch regardless.
	_, _ = page.Eval(ctx, `(() => { if (window.__c2a158 && window.__c2a158.real) window.fetch = window.__c2a158.real; return true; })()`)
	if strings.TrimSpace(calls) == "" || calls == "[]" {
		return fmt.Errorf("the bulk run issued no /test calls; the probe could not observe it")
	}
	var addressed []string
	for _, c := range strings.Split(strings.Trim(calls, "[]"), ",") {
		addressed = append(addressed, strings.Trim(strings.TrimSpace(c), "\""))
	}
	seen := map[string]bool{}
	for _, a := range addressed {
		seen[a] = true
	}
	if len(seen) > 1 {
		return fmt.Errorf("bulk test crossed platforms (issue #158): /test calls addressed %v -- "+
			"剩余探测在切平台后改道，收尾的计数与平台名都对不上", addressed)
	}
	if addressed[0] != "" && !strings.Contains(report, addressed[0]) && strings.Contains(report, "switched") {
		// The mid-run report confirms the switch happened; if every call still
		// addressed the original platform, the fix holds.
	}
	return nil
}
