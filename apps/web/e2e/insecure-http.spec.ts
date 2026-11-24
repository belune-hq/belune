import { expect, test, type Page } from "@playwright/test";
import { settled } from "./support/settled";

// Real production bytes through Caddy's HTTP catch-all. The setup project logs
// in on this origin; no cookie rewriting, API stubs or trustworthy-origin flags.
test.beforeEach(async ({ page, baseURL }, testInfo) => {
  await page.goto("/projects");
  await expect(page).toHaveURL(`${baseURL}/projects`);
  const capabilities = await page.evaluate(() => ({
    secureContext: window.isSecureContext,
    // eslint-disable-next-line no-restricted-syntax -- probe absence; do not call the API
    randomUUID: typeof crypto.randomUUID,
    // eslint-disable-next-line no-restricted-syntax -- probe absence; do not call the API
    clipboard: typeof navigator.clipboard,
  }));
  console.log("Non-loopback browser precondition:", capabilities);
  await testInfo.attach("http-capabilities", {
    body: JSON.stringify(capabilities),
    contentType: "application/json",
  });
  expect(capabilities).toEqual({
    secureContext: false,
    randomUUID: "undefined",
    clipboard: "undefined",
  });
});

async function api<T>(
  page: Page,
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  return page.evaluate(
    async ({ method, path, body }) => {
      const csrf = document.cookie.match(/(?:^|;\s*)csrf_token=([^;]+)/)?.[1];
      const response = await fetch(`/api${path}`, {
        method,
        headers: {
          "Content-Type": "application/json",
          ...(csrf ? { "X-CSRF-Token": csrf } : {}),
        },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (!response.ok)
        throw new Error(`${method} ${path}: ${response.status}`);
      return response.status === 204 ? null : response.json();
    },
    { method, path, body },
  ) as Promise<T>;
}

async function project(page: Page) {
  const stamp = Date.now();
  return api<{ id: string }>(page, "POST", "/projects", {
    name: `HTTP ${stamp}`,
    slug: `http-${stamp}`,
  });
}

test("project and application env drafts remain editable without randomUUID", async ({
  page,
}) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const p = await project(page);
  try {
    const app = await api<{ id: string }>(
      page,
      "POST",
      `/projects/${p.id}/applications`,
      {
        name: "HTTP App",
        type: "image",
        build_type: "image",
        source_image: "nginx:alpine",
      },
    );
    for (const path of [
      `/projects/${p.id}/env`,
      `/projects/${p.id}/applications/${app.id}/env`,
    ]) {
      await page.goto(path);
      await page.getByRole("button", { name: "Add", exact: true }).click();
      const key = page.getByPlaceholder("KEY", { exact: true });
      await expect(key).toHaveCount(1);
      await key.fill("HTTP_DRAFT");
      await page
        .getByPlaceholder("value", { exact: true })
        .fill("still-editable");
      await page.getByRole("button", { name: "Add", exact: true }).click();
      await expect(key).toHaveCount(2);
      await key.nth(1).fill("SECOND_DRAFT");
      await expect(key.first()).toHaveValue("HTTP_DRAFT");
      await expect(
        page.getByText("Something went wrong", { exact: true }),
      ).toHaveCount(0);
    }
    expect(errors).toEqual([]);
  } finally {
    await api(page, "DELETE", `/projects/${p.id}`);
  }
});

test("copying an env value visibly succeeds and the page stays interactive", async ({
  page,
}, testInfo) => {
  const p = await project(page);
  try {
    await api(page, "PUT", `/projects/${p.id}/env`, {
      vars: [{ key: "HTTP_COPY", value: "plain-http-copy", is_secret: false }],
    });
    await page.goto(`/projects/${p.id}/env`);
    await page.getByRole("button", { name: "Copy", exact: true }).click();
    await page
      .getByRole("menuitem", { name: "Copy value", exact: true })
      .click();
    const toast = page
      .locator("[data-sonner-toast]")
      .filter({ hasText: "Copied value" });
    await expect(toast).toBeVisible();
    const box = await settled(toast);
    expect(box.width).toBeGreaterThan(80);
    expect(box.height).toBeGreaterThan(20);
    await expect(page.getByText("Copy failed", { exact: true })).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath("http-copy.png") });
    await page.getByRole("button", { name: "Expand all", exact: true }).click();
    const value = page.getByPlaceholder("value", { exact: true });
    await value.fill("edited-after-copy");
    await expect(value).toHaveValue("edited-after-copy");
    await expect(
      page.getByText("Something went wrong", { exact: true }),
    ).toHaveCount(0);
  } finally {
    await api(page, "DELETE", `/projects/${p.id}`);
  }
});
