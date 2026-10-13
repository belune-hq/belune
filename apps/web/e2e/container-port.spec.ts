import { expect, test, type Locator, type Page } from "@playwright/test";

/**
 * The Port card on an application's Settings page, in a real browser.
 *
 * The jsdom suite covers the form's logic and the resolution text, but jsdom
 * has no layout engine: a card composited at opacity 0, or collapsed to no
 * height, satisfies every assertion there. This drill is what says a person can
 * actually see the card and the resolved ports, and that a saved port survives
 * a reload.
 *
 * Environment prepared by scripts/smoke-browser.sh.
 */
function required(name: string): string {
  const value = process.env[name];
  if (!value) {
    throw new Error(
      `${name} is not set — run this drill through scripts/smoke-browser.sh`,
    );
  }
  return value;
}

const EMAIL = required("BELUNE_E2E_EMAIL");
const PASSWORD = required("BELUNE_E2E_PASSWORD");

/** Effective opacity (the product down the ancestor chain) and box size, i.e.
 *  what jsdom cannot tell you. */
async function rendered(locator: Locator) {
  return locator.evaluate((el) => {
    let opacity = 1;
    for (let n: Element | null = el; n; n = n.parentElement) {
      opacity *= Number(getComputedStyle(n).opacity);
    }
    const r = el.getBoundingClientRect();
    return { opacity, width: r.width, height: r.height };
  });
}

/** Same-origin API call carrying the session cookie and CSRF token the way the
 *  dashboard's own client does. */
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

test("the Port card is visible, resolves ports, and a saved port survives a reload", async ({
  page,
}) => {
  await page.goto("/login");
  await page.locator("#email").fill(EMAIL);
  await page.locator("#password").fill(PASSWORD);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page).toHaveURL(/\/projects/);

  const stamp = Date.now();
  const project = await api<{ id: string }>(page, "POST", "/projects", {
    name: `Port ${stamp}`,
    slug: `port-${stamp}`,
  });
  const app = await api<{ id: string }>(
    page,
    "POST",
    `/projects/${project.id}/applications`,
    {
      name: "Two Ports",
      type: "image",
      build_type: "image",
      source_image: "nginx:latest",
    },
  );
  // Admin first, user second: the newest domain is the one a health check with
  // no app port would have followed.
  for (const [hostname, port] of [
    ["admin.port-drill.invalid", 3001],
    ["user.port-drill.invalid", 3000],
  ] as const) {
    await api(
      page,
      "POST",
      `/projects/${project.id}/applications/${app.id}/domains`,
      { hostname, container_port: port, ssl_mode: "off" },
    );
  }
  await api(
    page,
    "PUT",
    `/projects/${project.id}/applications/${app.id}/health-check`,
    {
      type: "http",
      path: "/up",
    },
  );

  await page.goto(`/projects/${project.id}/applications/${app.id}/settings`);

  // CardTitle is a div, not a heading; the description is unique to this card.
  const card = page.getByText(
    "The port your application listens on inside its container.",
  );
  await expect(card).toBeVisible();
  expect((await rendered(card)).opacity).toBe(1);
  const resolved = page.getByLabel("Resolved ports");
  await expect(resolved).toBeVisible();

  // Before: no app port, so the probe follows the newest domain (user → 3000).
  await expect(resolved).toContainText(":3000");
  await expect(resolved).toContainText("from the newest domain");

  // Real layout: the block occupies space and is not faded out.
  const before = await rendered(resolved);
  expect(before.opacity).toBe(1);
  expect(before.width).toBeGreaterThan(100);
  expect(before.height).toBeGreaterThan(20);

  // Pin the check to 3000 explicitly, then to 3001, to prove the probe moves
  // with the app port while each hostname keeps its own route.
  // Several cards on this page have a Save button; scope to this one's form.
  const save = () =>
    page
      .locator("form")
      .filter({ has: page.getByLabel("Container Port") })
      .getByRole("button", { name: "Save", exact: true })
      .click();
  const input = page.getByLabel("Container Port");
  await input.fill("3001");
  await save();
  await expect(page.getByText("Container port saved")).toBeVisible();

  await page.reload();
  await expect(page.getByLabel("Container Port")).toHaveValue("3001");
  const after = page.getByLabel("Resolved ports");
  await expect(after).toContainText("Health checks probe :3001");
  await expect(after).toContainText("user.port-drill.invalid → :3000");
  await expect(after).toContainText("admin.port-drill.invalid → :3001");
  expect((await rendered(after)).opacity).toBe(1);

  // Clearing it returns to following the domains.
  await page.getByLabel("Container Port").fill("");
  await save();
  await expect(page.getByText("Container port saved")).toBeVisible();
  await page.reload();
  await expect(page.getByLabel("Container Port")).toHaveValue("");
  await expect(page.getByLabel("Resolved ports")).toContainText(
    "from the newest domain",
  );
});
