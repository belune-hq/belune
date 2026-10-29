import { expect, test, type Page } from "@playwright/test";
import { settled } from "./support/settled";

/**
 * The blast-radius warning in front of WAF blocking, in a real browser.
 *
 * Turning blocking on is the action most likely to take a site down, and the
 * confirmation is the only place the operator is told which hostnames it
 * reaches and which stay detection-only. jsdom would pass `toBeVisible()` on a
 * dialog composited at opacity 0 (v0.1.16 shipped exactly that for a toast), so
 * this checks that the hostnames and the shared-host line are actually legible.
 *
 * It stops at the dialog: nothing is switched on, so the stack is left as found
 * and no request is ever refused.
 *
 * Environment prepared by scripts/smoke-browser.sh.
 */
async function api<T>(
  page: Page,
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  return page.evaluate(
    async ({ method, path, body }) => {
      const csrf = document.cookie.match(/(?:^|;\s*)csrf_token=([^;]+)/)?.[1];
      const res = await fetch(`/api${path}`, {
        method,
        headers: {
          "Content-Type": "application/json",
          ...(csrf ? { "X-CSRF-Token": csrf } : {}),
        },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (!res.ok) throw new Error(`${method} ${path}: ${res.status}`);
      return res.json();
    },
    { method, path, body },
  ) as Promise<T>;
}

test("the blocking confirmation names the hostnames and the shared one that only detects", async ({
  page,
}) => {
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/projects/);

  const stamp = Date.now();
  const project = await api<{ id: string }>(page, "POST", "/projects", {
    name: `WafBlock ${stamp}`,
    slug: `wafblock-${stamp}`,
  });
  const mkApp = (name: string) =>
    api<{ id: string }>(page, "POST", `/projects/${project.id}/applications`, {
      name,
      type: "image",
      build_type: "image",
      source_image: "nginx:latest",
    });
  const mine = await mkApp(`Opt In ${stamp}`);
  const sibling = await mkApp(`Sibling ${stamp}`);
  const domain = (appId: string, hostname: string, path?: string) =>
    api(page, "POST", `/projects/${project.id}/applications/${appId}/domains`, {
      hostname,
      path,
      ssl_mode: "off",
    });
  const own = `own-${stamp}.waf-drill.invalid`;
  const shared = `shared-${stamp}.waf-drill.invalid`;
  await domain(mine.id, own);
  await domain(mine.id, shared);
  // Another application serves the shared host by path, and has not opted in.
  await domain(sibling.id, shared, "/api");

  await page.goto("/proxy?tab=waf");
  const toggle = page.getByRole("switch", {
    name: `Block requests for Opt In ${stamp}`,
  });
  await expect(toggle).toBeVisible();
  await toggle.click();

  const dialog = page.getByRole("alertdialog");
  await expect(dialog).toBeVisible();
  await settled(dialog);

  // Each line is on screen and legible, not merely in the DOM.
  for (const text of [own, /Still detection only/, shared, /403/]) {
    const el = dialog.getByText(text).first();
    await expect(el).toBeVisible();
    const box = await settled(el);
    expect(box.width).toBeGreaterThan(0);
    expect(box.height).toBeGreaterThan(0);
  }

  // Cancelling changes nothing: the switch is still off.
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(dialog).toBeHidden();
  await expect(toggle).not.toBeChecked();
});
