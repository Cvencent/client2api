async (page) => {
  const out = "D:/client2api-src/output/playwright";
  const errors = [];
  page.on("pageerror", e => errors.push(String(e && e.message || e)));
  page.on("console", m => { if (m.type() === "error") errors.push("console: " + m.text()); });

  const sleep = ms => page.waitForTimeout(ms);

  async function metrics() {
    return await page.evaluate(() => {
      const visibleView = Array.from(document.querySelectorAll("section.view")).find(v => !v.hidden);
      const savebar = visibleView && visibleView.querySelector(".savebar");
      const topbar = document.querySelector(".topbar");
      const nav = document.querySelector(".nav");
      const rect = el => {
        if (!el) return null;
        const r = el.getBoundingClientRect();
        return { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height), bottom: Math.round(r.bottom), right: Math.round(r.right) };
      };
      return {
        view: visibleView && visibleView.id,
        dataTheme: document.documentElement.getAttribute("data-theme") || "dark",
        viewport: { w: innerWidth, h: innerHeight },
        doc: { scrollWidth: document.documentElement.scrollWidth, clientWidth: document.documentElement.clientWidth, scrollHeight: document.documentElement.scrollHeight },
        topbar: rect(topbar),
        nav: rect(nav),
        viewRect: rect(visibleView),
        savebar: savebar ? { rect: rect(savebar), position: getComputedStyle(savebar).position, bottom: getComputedStyle(savebar).bottom } : null,
        activeNav: document.querySelector(".nav a.on")?.getAttribute("data-view") || null,
        visibleSubtabs: Array.from(document.querySelectorAll(".subtab-panel")).filter(p => !p.hidden).map(p => p.id),
      };
    });
  }

  async function shot(name, fullPage = false) {
    await page.screenshot({ path: out + "/" + name, fullPage });
  }

  async function openView(name) {
    await page.locator('a[data-view="' + name + '"]').click();
    await sleep(450);
    await page.evaluate(() => window.scrollTo(0, 0));
    await sleep(80);
    return await metrics();
  }

  await page.setViewportSize({ width: 1440, height: 900 });
  await page.emulateMedia({ reducedMotion: "no-preference" });
  await page.goto("http://127.0.0.1:8891/panel/", { waitUntil: "networkidle" });
  await page.evaluate(() => { localStorage.removeItem("c2a.theme"); });
  await page.reload({ waitUntil: "networkidle" });
  await sleep(250);

  const source = await page.content();
  if (!source.includes("Unified Apple-like workspace layer")) errors.push("design layer not present in served page");

  const report = { initial: await metrics(), views: {}, subtabs: {}, dialog: {}, themes: {}, responsive: {}, savebars: {}, errors };
  const views = ["accounts", "chat", "usage", "alerts", "credits", "packages", "taskscenter", "models", "platforms", "config", "logs"];
  for (const name of views) {
    report.views[name] = await openView(name);
    await shot("qa-1440-" + name + ".png", true);
  }

  await openView("usage");
  await page.locator("#usage-tab-recent").click();
  await sleep(300);
  report.subtabs.usageRecent = { metrics: await metrics(), heading: await page.locator("#usage-panel-recent h3").first().textContent() };
  await shot("qa-1440-usage-recent.png", true);
  await page.locator("#usage-tab-stats").click();
  await sleep(300);
  report.subtabs.usageStats = { metrics: await metrics(), heading: await page.locator("#usage-panel-stats h3").first().textContent() };
  await shot("qa-1440-usage-stats.png", true);

  await openView("taskscenter");
  await page.locator("#tasks-tab-batch").click();
  await sleep(300);
  report.subtabs.tasksBatch = { metrics: await metrics(), heading: await page.locator("#tasks-panel-batch h3").first().textContent() };
  await shot("qa-1440-tasks-batch.png", true);
  await page.locator("#tasks-tab-board").click();
  await sleep(300);
  report.subtabs.tasksBoard = { metrics: await metrics(), heading: await page.locator("#tasks-panel-board h3").first().textContent() };
  await shot("qa-1440-tasks-board.png", true);

  await openView("accounts");
  await page.locator("#btnAdd").click();
  await sleep(300);
  report.dialog.add = await page.evaluate(() => {
    const veil = document.querySelector("#addVeil");
    const dlg = veil && veil.querySelector(".dlg");
    const r = dlg && dlg.getBoundingClientRect();
    return { open: !!veil && veil.classList.contains("on"), display: veil && getComputedStyle(veil).display, dialog: r && { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) } };
  });
  await shot("qa-1440-dialog-add.png", false);
  await page.locator("#btnCloseAdd").click();
  await sleep(200);

  for (let i = 0; i < 3; i++) {
    await page.locator("#btnTheme").click();
    await sleep(180);
    const t = await page.evaluate(() => document.documentElement.getAttribute("data-theme") || "dark");
    report.themes["step" + (i + 1)] = { theme: t, metrics: await metrics() };
    if (t === "light") await shot("qa-1440-accounts-light.png", true);
  }

  await page.emulateMedia({ reducedMotion: "reduce" });
  await sleep(100);
  report.responsive.reducedMotion = await page.evaluate(() => {
    const v = document.querySelector("section.view:not([hidden])");
    return { media: matchMedia("(prefers-reduced-motion: reduce)").matches, animation: getComputedStyle(v).animationName, transition: getComputedStyle(document.querySelector("button")).transitionDuration };
  });
  await page.emulateMedia({ reducedMotion: "no-preference" });

  await page.setViewportSize({ width: 375, height: 812 });
  await openView("accounts");
  report.responsive.mobile375 = await metrics();
  await shot("qa-375-accounts.png", true);
  await page.setViewportSize({ width: 768, height: 900 });
  await openView("platforms");
  report.responsive.tablet768 = await metrics();
  await shot("qa-768-platforms.png", true);
  await page.setViewportSize({ width: 2200, height: 1100 });
  await openView("models");
  report.responsive.wide2200 = await metrics();
  await shot("qa-2200-models.png", true);

  await page.setViewportSize({ width: 1440, height: 900 });
  for (const name of ["platforms", "config"]) {
    await openView(name);
    await page.evaluate(() => window.scrollTo(0, Math.min(700, document.documentElement.scrollHeight - innerHeight)));
    await sleep(150);
    report.savebars[name] = await metrics();
  }

  return report;
}
