package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"client2api/internal/browser"
)

// assertA11yTablesAndPanels measures two accessibility guarantees in the AX
// tree and live DOM (issues #151/#152).
//
// #151: a tabpanel whose content has no focusable element must be focusable
// itself (WAI-ARIA APG Tabs), or Tab jumps straight over the whole content --
// the usage "统计明细" panel is the panel's only pure-text analysis page and
// was permanently unreachable by keyboard.
//
// #152: every data table must carry an accessible name and every column
// header a scope, or a screen reader reads a bare "table" and cells without
// their column.
//
// Both are measured in the browser because they are properties of the rendered
// DOM and the AX tree, not of the source text.
func assertA11yTablesAndPanels(ctx context.Context, page *browser.Page) error {
	if page == nil {
		return nil
	}
	// Give the usage view a visit so its four tables exist in the live DOM
	// (they are static in the template, but the panel lazily populates data).
	if _, err := page.Eval(ctx, `(() => { if (typeof go === "function") go("usage"); return true; })()`); err != nil {
		return fmt.Errorf("open the usage view: %w", err)
	}
	if ok, err := page.WaitFor(ctx,
		`(() => document.querySelectorAll("#view-usage table").length >= 4)()`,
		15*time.Second); err != nil {
		return fmt.Errorf("wait for the usage tables: %w", err)
	} else if !ok {
		return fmt.Errorf("the usage tables never rendered")
	}
	// Select the "统计明细" subtab FIRST (issue #151): it is the one panel
	// with no focusable content, and the one whose reachability is at stake.
	// While it is hidden, the panel-focus check would silently skip it.
	if _, err := page.Eval(ctx, `(() => {
	  const t = document.querySelector("#usage-tab-stats");
	  if (t) t.click();
	  return true;
	})()`); err != nil {
		return fmt.Errorf("select the stats subtab: %w", err)
	}
	if ok, err := page.WaitFor(ctx,
		`(() => { const p = document.querySelector("#usage-panel-stats"); return !!(p && !p.hidden); })()`,
		10*time.Second); err != nil {
		return fmt.Errorf("wait for the stats panel: %w", err)
	} else if !ok {
		return fmt.Errorf("the stats panel never became visible")
	}
	report, err := page.EvalString(ctx, `(() => {
	  const problems = [];
	  const focusableSel = "a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]";

	  // 1. Every visible tabpanel: either it has focusable content, or it is
	  //    focusable itself (#151).
	  document.querySelectorAll("[role=tabpanel]").forEach(p => {
	    if (p.offsetParent === null) return; // hidden panel, not reachable now
	    if (p.querySelector(focusableSel)) return;
	    if (p.getAttribute("tabindex") !== "0") {
	      problems.push("tabpanel " + (p.id || "(unnamed)") +
	        " 内容里没有可聚焦元素且自身不可聚焦：Tab 会整块跳过（#151）");
	    }
	  });

	  // 2. Every table: an accessible name; every th: a scope (#152).
	  let tables = 0, unnamed = 0, ths = 0, unscoped = 0;
	  document.querySelectorAll("table").forEach(tb => {
	    tables++;
	    const name = tb.caption || tb.getAttribute("aria-label") || tb.getAttribute("aria-labelledby");
	    if (!name) { unnamed++; return; }
	    tb.querySelectorAll("th").forEach(th => {
	      ths++;
	      if (!th.getAttribute("scope")) unscoped++;
	    });
	  });
	  if (unnamed > 0) {
	    problems.push(unnamed + "/" + tables + " 张表格没有可访问名称（读屏只念「表格」，#152）");
	  }
	  if (unscoped > 0) {
	    problems.push(ths + " 个列头里 " + unscoped + " 个没有 scope：单元格导航不报列名（#152）");
	  }
	  if (tables === 0) {
	    problems.push("页面上量不到任何表格，检查无从进行");
	  }
	  return problems.join(" || ");
	})()`)
	if err != nil {
		return fmt.Errorf("measure the a11y tables/panels: %w", err)
	}
	if strings.TrimSpace(report) != "" {
		return fmt.Errorf("a11y regression:\n  %s", strings.Join(strings.Split(report, " || "), "\n  "))
	}
	return nil
}
