import { expect, test, type Locator, type Page } from "@playwright/test";

/**
 * The Container Port field on the Health Check card of an application's
 * Settings page, in a real browser.
 *
 * jsdom has no layout engine: a field composited at opacity 0, or collapsed to
 * no height, satisfies every assertion there. This drill is what says a person
 * can actually see the field and the probe line, that the field is not tied to
 * the HTTP method, and that a saved port survives a reload.
 *
 * Environment prepared by scripts/smoke-browser.sh.
 */
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

test("the Container Port field is visible, saves with the health check, and survives a reload", async ({
  page,
}) => {
  // Already signed in: auth.setup.ts does it once for the whole run.
  await page.goto("/projects");
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
    "How the platform decides the application is healthy.",
  );
  await expect(card).toBeVisible();
  expect((await rendered(card)).opacity).toBe(1);

  const input = page.getByLabel("Container Port");
  await expect(input).toBeVisible();
  const field = await rendered(input);
  expect(field.opacity).toBe(1);
  expect(field.width).toBeGreaterThan(50);
  expect(field.height).toBeGreaterThan(15);

  // Before: no app port, so the probe follows the newest domain (user → 3000).
  const probe = page.getByText("Health checks probe");
  await expect(probe).toContainText(":3000");
  await expect(probe).toContainText("from the newest domain");
  const probeBox = await rendered(probe);
  expect(probeBox.opacity).toBe(1);
  expect(probeBox.width).toBeGreaterThan(100);
  expect(probeBox.height).toBeGreaterThan(8);

  // The port belongs to the card, not to the HTTP method: it must stay put
  // when the method changes, or apps with no HTTP check could not set it.
  await page.getByRole("button", { name: "None", exact: true }).click();
  await expect(input).toBeVisible();
  await page.getByRole("button", { name: "HTTP", exact: true }).click();

  // Several cards on this page have a Save button; scope to this one's form.
  const save = () =>
    page
      .locator("form")
      .filter({ has: page.getByLabel("Container Port") })
      .getByRole("button", { name: "Save", exact: true })
      .click();
  const saved = page.getByText("Health check saved");

  await input.fill("3001");
  await save();
  await expect(saved).toBeVisible();

  await page.reload();
  await expect(page.getByLabel("Container Port")).toHaveValue("3001");
  const after = page.getByText("Health checks probe");
  await expect(after).toContainText("Health checks probe :3001");
  await expect(after).toContainText("(this port)");
  expect((await rendered(after)).opacity).toBe(1);

  // Clearing it returns to following the domains.
  await page.getByLabel("Container Port").fill("");
  await save();
  await expect(page.getByText("Health check saved")).toBeVisible();
  await page.reload();
  await expect(page.getByLabel("Container Port")).toHaveValue("");
  await expect(page.getByText("Health checks probe")).toContainText(
    "from the newest domain",
  );
});
