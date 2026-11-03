package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"client2api/internal/browser"
)

// assertStatsResizesWithText reproduces issue #159 in the live page: scaling
// the root font to 200% (the WCAG 1.4.4 "resize text" path, which does not
// change the viewport) must not clip the stats-band labels.
//
// The reported shape: the band's wrap decision was gated on viewport width
// (max-width: 1160px) while min-width was absolute px, so enlarged labels got
// ellipsis-trimmed (9/10 truncated at 200%) and two of the tiles had no title
// at all.  After the fix the band wraps and the tiles size in em, so nothing
// clips at any font scale.
//
// The probe works against the rendered band with two measurements: first
// assert the titles exist at 100%, then measure clipping at 200%.
func assertStatsResizesWithText(ctx context.Context, page *browser.Page) error {
	if page == nil {
		return nil
	}
	if _, err := page.Eval(ctx, `(() => { if (typeof go === "function") go("accounts"); return true; })()`); err != nil {
		return fmt.Errorf("open the accounts view: %w", err)
	}
	// Wait for the band to render at least one real frame.
	if ok, err := page.WaitFor(ctx,
		`(() => { const t = document.querySelector("#sTotal"); return !!(t && t.textContent && t.textContent !== "-"); })()`,
		15*time.Second); err != nil {
		return fmt.Errorf("wait for the stats band: %w", err)
	} else if !ok {
		return fmt.Errorf("the stats band never rendered a number")
	}

	// 1) Titles at 100%: every .stat must carry a title (the two div tiles had
	//    none, and the eight button tiles' titles did not name their label).
	report, err := page.EvalString(ctx, `(() => {
	  const problems = [];
	  const stats = [...document.querySelectorAll("#view-accounts .stats > .stat")];
	  if (!stats.length) problems.push("统计带没有任何格子");
	  for (const st of stats) {
	    const k = st.querySelector(".k");
	    if (!k || !st.getAttribute("title")) {
	      problems.push("格子 " + (k ? k.textContent : "?") + " 没有 title：200% 文字路径下被省略号截断后将无任何可读回入口（issue #159）");
	    }
	  }
	  return problems.join(" || ");
	})()`)
	if err != nil {
		return fmt.Errorf("measure the stats titles: %w", err)
	}
	if strings.TrimSpace(report) != "" {
		return fmt.Errorf("stats-band titles:\n  %s", strings.Join(strings.Split(report, " || "), "\n  "))
	}

	// 2) 200% text: the band must either fit (no clipping) or have wrapped —
	//    in both cases no label is ellipsis-trimmed.
	if _, err := page.Eval(ctx, `(() => { document.documentElement.style.fontSize = "200%"; return true; })()`); err != nil {
		return fmt.Errorf("scale the root font to 200 percent: %w", err)
	}
	defer func() {
		_, _ = page.Eval(ctx, `(() => { document.documentElement.style.fontSize = ""; return true; })()`)
	}()
	time.Sleep(600 * time.Millisecond)
	report, err = page.EvalString(ctx, `(() => {
	  const problems = [];
	  let clipped = 0, worst = 0, worstLabel = "?";
	  const wrappedRows = new Set();
	  for (const st of document.querySelectorAll("#view-accounts .stats > .stat")) {
	    const k = st.querySelector(".k");
	    if (!k) continue;
	    const r = st.getBoundingClientRect();
	    wrappedRows.add(Math.round(r.top));
	    const over = k.scrollWidth - k.clientWidth;
	    if (over > 1) {
	      clipped++;
	      if (over > worst) { worst = over; worstLabel = k.textContent; }
	    }
	  }
	  if (clipped > 0) problems.push(clipped + "/10 个标签在 200% 字号下被截断（最狠 " + worstLabel + " 超 " + worst + "px）：换行判据仍卡在视口宽度、格子宽度仍是绝对像素（issue #159）");
	  return problems.join(" || ") + " (rows=" + wrappedRows.size + ")";
	})()`)
	if err != nil {
		return fmt.Errorf("measure the 200%% clipping: %w", err)
	}
	if strings.TrimSpace(strings.Split(report, " (rows=")[0]) != "" {
		return fmt.Errorf("stats-band 200%% text clipping (issue #159):\n  %s", report)
	}
	return nil
}
