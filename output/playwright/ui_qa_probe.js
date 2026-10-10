async (page) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  return {
    title: await page.title(),
    width: await page.evaluate(() => window.innerWidth),
  };
}
