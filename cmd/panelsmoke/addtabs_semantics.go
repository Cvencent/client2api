package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"client2api/internal/browser"
)

// assertAddTabsSemantics measures the add-account dialog's three-mode switch in
// the AX tree and with real key events (issue #150).
//
// The three modes (browser login / import / manual) used to be three bare
// buttons: no tablist/tab semantics, no aria-selected, arrow keys dead --
// while four sibling subtab groups in the same file (wired through
// setupSubtabs) had all of it.  A screen reader heard three identical
// buttons; a keyboard user burned three Tab stops and got nothing from the
// arrows.
//
// Measured in the browser, not asserted against the source: the question is
// "what does the AX tree expose and where does focus go", which only the
// layout engine and the accessibility tree can answer.  The dialog is opened
// the way a user opens it (clicking the launcher), then the properties and
// key handling are read back.
func assertAddTabsSemantics(ctx context.Context, page *browser.Page) error {
	if page == nil {
		return nil
	}
	// Open the dialog the way the shell does: #btnAdd only lives on the
	// accounts view (it is hidden elsewhere), so land there first, then click.
	if _, err := page.Eval(ctx, `(() => { if (typeof go === "function") go("accounts"); return true; })()`); err != nil {
		return fmt.Errorf("switch to the accounts view: %w", err)
	}
	if _, err := page.Eval(ctx, `(() => { const b = document.querySelector("#btnAdd"); if (b && !b.hidden) b.click(); return true; })()`); err != nil {
		return fmt.Errorf("open the add-account dialog: %w", err)
	}
	if ok, err := page.WaitFor(ctx,
		`(() => { const v = document.querySelector("#addVeil"); return !!(v && !v.hidden && v.classList.contains("on")); })()`,
		10*time.Second); err != nil {
		return fmt.Errorf("wait for the add-account veil: %w", err)
	} else if !ok {
		return fmt.Errorf("the add-account veil never opened")
	}
	report, err := page.EvalString(ctx, `(() => {
	  const problems = [];
	  const list = document.querySelector("#addTabs");
	  if (!list) return "找不到 #addTabs，语义检查无从进行";
	  const tabs = Array.from(list.querySelectorAll("[data-tab]"));
	  if (tabs.length < 2) return "#addTabs 里的档位不足两个";

	  // 1. List semantics: role=tablist on the container, role=tab on each.
	  if (list.getAttribute("role") !== "tablist") {
	    problems.push("#addTabs 没有 role=tablist：读屏把三档读成三颗普通按钮（#150）");
	  }
	  tabs.forEach(t => {
	    if (t.getAttribute("role") !== "tab") {
	      problems.push("档位 " + (t.textContent || "").trim() + " 没有 role=tab");
	    }
	    if (!t.getAttribute("aria-controls")) {
	      problems.push("档位 " + (t.textContent || "").trim() + " 没有 aria-controls：读屏不知道它控制哪块面板");
	    }
	  });

	  // 2. Selected state must be exposed, exactly one selected.
	  const selected = tabs.filter(t => t.getAttribute("aria-selected") === "true");
	  if (selected.length !== 1) {
	    problems.push("aria-selected=true 的档位有 " + selected.length + " 个，必须恰好一个");
	  }

	  // 3. Roving tabindex: exactly one stop, on the selected tab.
	  const stops = tabs.filter(t => t.tabIndex === 0);
	  if (stops.length !== 1) {
	    problems.push("tabIndex=0 的档位有 " + stops.length + " 个：roving tabindex 应该只留一个停靠点（#150：三颗都是 0，Tab 序白走三下）");
	  }

	  // 4. Arrow keys move both selection and focus, like the four sibling
	  //    groups wired through setupSubtabs.
	  const dispatch = (key) => {
	    const ev = new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true });
	    (document.activeElement || list).dispatchEvent(ev);
	    return ev;
	  };
	  const start = tabs.find(t => t.getAttribute("aria-selected") === "true") || tabs[0];
	  start.focus();
	  dispatch("ArrowRight");
	  const afterRight = tabs.find(t => t.getAttribute("aria-selected") === "true");
  if (!afterRight || afterRight === start) {
	    problems.push("ArrowRight 后选中档没有移动（#150：方向键完全无效，与同页做对的四组行为相反）");
	  } else if (document.activeElement !== afterRight) {
	    problems.push("ArrowRight 后焦点没有跟到新选中的档位上");
	  }
	  dispatch("ArrowLeft");
	  const afterLeft = tabs.find(t => t.getAttribute("aria-selected") === "true");
	  if (!afterLeft || afterLeft !== start) {
	    problems.push("ArrowLeft 没有回到起始档位");
	  }
	  return problems.join(" || ");
	})()`)
	if err != nil {
		return fmt.Errorf("measure the add-tabs semantics: %w", err)
	}
	if strings.TrimSpace(report) != "" {
		return fmt.Errorf("add-tabs semantics regression:\n  %s", strings.Join(strings.Split(report, " || "), "\n  "))
	}
	return nil
}
