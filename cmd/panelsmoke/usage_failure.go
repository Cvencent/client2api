package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"client2api/internal/browser"
)

// assertUsageReadFailure reproduces issue #156 in the live page: wrap
// window.fetch so /panel/api/usage fails, re-run renderUsage, and measure that
// every surface that carries a number went with it -- not just the chart.
//
// The reported shape: the error branch cleared the chart, tiles and readout
// but left the four detail tables, the 请求/失败 readout, the recent-call
// subtable and the page-head numbers showing the PREVIOUS frame, while the
// only "获取失败" hint lived in another subtab's header (invisible the moment
// the operator switched).  Any single stale number is a regression.
func assertUsageReadFailure(ctx context.Context, page *browser.Page) error {
	if page == nil {
		return nil
	}
	if _, err := page.Eval(ctx, `(() => { if (typeof go === "function") go("usage"); return true; })()`); err != nil {
		return fmt.Errorf("open the usage view: %w", err)
	}
	// Wait for a successful render first, so the page is holding a real frame
	// (the stale-data shape can only be proven against something stale).
	if ok, err := page.WaitFor(ctx,
		`(() => { const t = document.querySelector("#usTiles"); return !!(t && t.children.length > 0); })()`,
		15*time.Second); err != nil {
		return fmt.Errorf("wait for the first usage render: %w", err)
	} else if !ok {
		return fmt.Errorf("the usage tiles never rendered")
	}
	// Inject the failure and re-render.
	if _, err := page.Eval(ctx, `(() => {
	  window.__c2aRealFetch = window.fetch;
	  window.fetch = function(input, init) {
	    const url = typeof input === "string" ? input : (input && input.url) || "";
	    if (String(url).indexOf("/panel/api/usage") >= 0) {
	      return Promise.reject(new TypeError("Failed to fetch"));
	    }
	    return window.__c2aRealFetch.apply(this, arguments);
	  };
	  if (typeof renderUsage === "function") renderUsage();
	  return true;
	})()`); err != nil {
		return fmt.Errorf("inject the usage failure: %w", err)
	}
	// Let the failed render settle: wait for the error hint to land.  The
	// hint text is "获取失败：" + r.err, and a rejected fetch names itself.
	if ok, err := page.WaitFor(ctx,
		`(() => { const n = document.querySelector("#usNote"); return !!(n && n.textContent && n.textContent.length > 0 && n.textContent.indexOf("实时读取") < 0 && n.textContent.indexOf("窗口") < 0); })()`,
		10*time.Second); err != nil {
		return fmt.Errorf("wait for the failed render: %w", err)
	} else if !ok {
		return fmt.Errorf("the failed render never surfaced its error hint")
	}
	report, err := page.EvalString(ctx, `(() => {
	  const problems = [];
	  const rows = id => document.getElementById(id) ? document.getElementById(id).children.length : -1;
	  // 1. The four detail tables and the recent subtable must not hold rows.
	  for (const id of ["usClientBody", "usAccBody", "usModelBody", "usRealmBody", "usRecentBody"]) {
	    const n = rows(id);
	    if (n > 1) problems.push(id + " 仍有 " + n + " 行上一帧数据（#156：读失败时明细表必须清掉）");
	  }
	  // 2. The 请求/失败 readout must be reset, not stale.
	  const stats = document.querySelector("#usStats");
	  if (stats && stats.textContent && stats.textContent !== "-" && stats.textContent.indexOf("/") >= 0
	      && /\d/.test(stats.textContent)) {
	    problems.push("#usStats 仍是上一帧读数 " + JSON.stringify(stats.textContent) + "（#156）");
	  }
	  // 3. The recent subtable's "N / N 条" note must be gone.
	  const note = document.querySelector("#usRecentNote");
	  if (note && /\d\s*\/\s*\d/.test(note.textContent)) {
	    problems.push("#usRecentNote 仍是 " + JSON.stringify(note.textContent) + "（#156）");
	  }
	  // 4. The error hint must be visible IN THIS SUBTAB too (selecting the
	  //    stats subtab is what hid #usNote in the report).
	  const sel = document.querySelector("#usStatsErr");
	  if (!sel || sel.hidden || !sel.textContent) {
	    problems.push("统计明细子页没有可见的错误提示（#156：#usNote 挂在别的子页里，换子页即不可见）");
	  }
	  // 5. Restore the real fetch and re-render, leaving the page healthy.
	  window.fetch = window.__c2aRealFetch;
	  if (typeof renderUsage === "function") renderUsage();
	  return problems.join(" || ");
	})()`)
	if err != nil {
		return fmt.Errorf("measure the usage failure surfaces: %w", err)
	}
	if strings.TrimSpace(report) != "" {
		return fmt.Errorf("usage read-failure regression:\n  %s", strings.Join(strings.Split(report, " || "), "\n  "))
	}
	return nil
}
