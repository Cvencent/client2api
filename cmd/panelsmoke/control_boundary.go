package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"client2api/internal/browser"
)

// assertControlBoundariesAreVisible measures WCAG 2.2 AA 1.4.11 for the
// resting state of every text control (issue #220).
//
// The control styling deliberately drops the box and keeps a single 1px
// baseline ("像仪器面板上待写入的槽位"), which is a fine design -- but the line
// was drawn with --line, the token that also serves decorative card borders
// and table separators.  Against the field background that measured 1.1:1 in
// both themes, so the resting control had no visible boundary at all: the
// outline only appeared once the pointer hovered it or the field was focused.
//
// Component boundaries are information required to identify the control, so
// 1.4.11 asks for 3:1.  The assertion composites the declared colours over the
// nearest opaque ancestor background and checks the worst visible control in
// the config view in both themes.  It is measured in the browser because the
// answer depends on what the cascade actually resolved to, not on the source.
func assertControlBoundariesAreVisible(ctx context.Context, page *browser.Page) error {
	if page == nil {
		return nil
	}
	if _, err := page.Eval(ctx, `(() => { if (typeof go === "function") go("config"); return true; })()`); err != nil {
		return fmt.Errorf("open the config view: %w", err)
	}
	if ok, err := page.WaitFor(ctx,
		`(() => document.querySelectorAll("#view-config input, #view-config select, #view-config textarea").length > 0)()`,
		15*time.Second); err != nil {
		return fmt.Errorf("wait for the config controls: %w", err)
	} else if !ok {
		return fmt.Errorf("the config view never rendered any text control")
	}
	report, err := page.EvalString(ctx, `(() => {
	  const parse = (s) => { const m = String(s).match(/[\d.]+/g) || []; return { r: +m[0], g: +m[1], b: +m[2], a: m[3] === undefined ? 1 : +m[3] }; };
	  const over = (fg, bg) => [0, 1, 2].map(i => Math.round(fg[["r","g","b"][i]] * fg.a + bg[i] * (1 - fg.a)));
	  const srgb = (c) => { c /= 255; return c <= 0.04045 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4); };
	  const lum = (c) => 0.2126 * srgb(c[0]) + 0.7152 * srgb(c[1]) + 0.0722 * srgb(c[2]);
	  const ratio = (a, b) => { const la = lum(a), lb = lum(b); const hi = Math.max(la, lb), lo = Math.min(la, lb); return (hi + 0.05) / (lo + 0.05); };
	  const opaqueBg = (el) => {
	    let n = el;
	    while (n && n !== document.documentElement) {
	      const c = parse(getComputedStyle(n).backgroundColor);
	      if (c.a >= 0.999) return [c.r, c.g, c.b];
	      n = n.parentElement;
	    }
	    return [255, 255, 255];
	  };
	  const worst = (theme) => {
	    document.documentElement.dataset.theme = theme;
	    const els = [...document.querySelectorAll("#view-config input, #view-config select, #view-config textarea")]
	      .filter(e => e.offsetParent !== null && !/^(checkbox|radio)$/.test(e.type || ""));
	    let min = null, who = null;
	    for (const el of els) {
	      const cs = getComputedStyle(el);
	      const cardBg = opaqueBg(el.parentElement);
	      const fieldBg = over(parse(cs.backgroundColor), cardBg);
	      const line = over(parse(cs.borderBottomColor), cardBg);
	      const r = ratio(line, fieldBg);
	      if (min === null || r < min) { min = r; who = (el.id || el.tagName); }
	    }
	    return { theme, count: els.length, min: min === null ? null : Math.round(min * 100) / 100, el: who };
	  };
	  const dark = worst("dark");
	  const light = worst("light");
	  document.documentElement.dataset.theme = "dark";
	  return JSON.stringify({ dark, light });
	})()`)
	if err != nil {
		return fmt.Errorf("measure control boundaries: %w", err)
	}
	type themeReport struct {
		Theme string  `json:"theme"`
		Count int     `json:"count"`
		Min   float64 `json:"min"`
		El    string  `json:"el"`
	}
	var parsed struct {
		Dark  themeReport `json:"dark"`
		Light themeReport `json:"light"`
	}
	if err := json.Unmarshal([]byte(report), &parsed); err != nil {
		return fmt.Errorf("parse the boundary report %q: %w", report, err)
	}
	if parsed.Dark.Count == 0 || parsed.Light.Count == 0 {
		return fmt.Errorf("no visible text control measured (dark=%d light=%d)",
			parsed.Dark.Count, parsed.Light.Count)
	}
	const want = 3.0
	if parsed.Dark.Min < want {
		return fmt.Errorf("dark theme: %s has a resting boundary of %.2f:1, want >= %.1f:1 "+
			"(WCAG 1.4.11; the control border must not reuse the decorative --line) (issue #220)",
			parsed.Dark.El, parsed.Dark.Min, want)
	}
	if parsed.Light.Min < want {
		return fmt.Errorf("light theme: %s has a resting boundary of %.2f:1, want >= %.1f:1 "+
			"(WCAG 1.4.11) (issue #220)", parsed.Light.El, parsed.Light.Min, want)
	}
	return nil
}
