import { expect, type Locator } from "@playwright/test";

/** Effective opacity (the product down the ancestor chain) and box size, i.e.
 *  what jsdom cannot tell you. */
export async function rendered(locator: Locator) {
  return locator.evaluate((el) => {
    let opacity = 1;
    for (let n: Element | null = el; n; n = n.parentElement) {
      opacity *= Number(getComputedStyle(n).opacity);
    }
    const r = el.getBoundingClientRect();
    return { opacity, width: r.width, height: r.height };
  });
}

/**
 * Waits for the ancestor-chain opacity to settle at 1, then returns the box.
 *
 * ⚠️ Dialogs and tooltips here animate in (`data-open:animate-in fade-in-0
 * zoom-in-95`), so measuring immediately after toBeVisible() catches the fade
 * mid-flight and reports a partial opacity — which is indistinguishable from
 * the v0.1.16 "composited at opacity 0" bug this whole helper exists to catch.
 * The plain rendered() check is only safe on a settled page.
 */
export async function settled(locator: Locator) {
  await expect
    .poll(async () => (await rendered(locator)).opacity, { timeout: 5000 })
    .toBe(1);
  return rendered(locator);
}
