import { expect, test, type Page } from "@playwright/test";
import { rendered, settled } from "./support/settled";

/**
 * The Processes tab, in a real browser.
 *
 * What jsdom cannot answer: that the rows, the status pills and the form are
 * actually on screen at full opacity, and that the routable type really offers no
 * way to delete it. v0.1.16 shipped a toast composited at opacity 0, which every
 * jsdom assertion — toBeVisible included — passed.
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

test("the Processes tab shows the routable type, adds a worker, and offers no way to delete web", async ({
  page,
}) => {
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/projects/);

  const stamp = Date.now();
  const project = await api<{ id: string }>(page, "POST", "/projects", {
    name: `Processes ${stamp}`,
    slug: `processes-${stamp}`,
  });
  const app = await api<{ id: string; slug: string }>(
    page,
    "POST",
    `/projects/${project.id}/applications`,
    {
      name: "Worker Host",
      type: "image",
      build_type: "image",
      source_image: "nginx:latest",
    },
  );

  await page.goto(`/projects/${project.id}/applications/${app.id}/processes`);

  // The tab exists and the page is really on screen.
  // exact: the project is named "Processes <stamp>", so its breadcrumb link
  // would otherwise match too.
  await expect(
    page.getByRole("link", { name: "Processes", exact: true }),
  ).toBeVisible();
  const heading = page.getByText("Process Types", { exact: true }).first();
  await expect(heading).toBeVisible();
  expect((await settled(heading)).height).toBeGreaterThan(10);

  // The backfilled web type: routable, inheriting the application's check, with
  // its container, and NO remove control.
  const webRow = page.getByRole("listitem").filter({ hasText: "Routable" });
  await expect(webRow).toBeVisible();
  // This application was created with no health check path, so the inherited http
  // check probes nothing. The row must SAY so, visibly (a jsdom run cannot tell
  // opacity 0 from shown), rather than read as a configured check.
  const inert = webRow.getByTestId("health-inert");
  await expect(inert).toContainText("no path set, so nothing is checked");
  expect((await settled(inert)).height).toBeGreaterThan(10);
  expect((await rendered(inert)).opacity).toBe(1);
  await expect(webRow.getByText(app.slug, { exact: true })).toBeVisible();
  await expect(webRow.getByRole("button", { name: "Edit web" })).toBeVisible();
  await expect(webRow.getByRole("button", { name: /^Remove/ })).toHaveCount(0);
  await expect(webRow.getByText(/cannot be removed/i)).toBeVisible();

  // The notice about when a change takes effect is readable, not just present.
  const notice = page.getByText(/next deploy/i).first();
  await expect(notice).toBeVisible();
  expect((await rendered(notice)).opacity).toBe(1);

  // Add a worker through the form.
  await page.getByRole("button", { name: /Add Process Type/ }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();
  await settled(dialog);

  // An invalid name is not flagged while it is being typed...
  const name = dialog.getByLabel("Name", { exact: true });
  await name.fill("Bad_Name");
  await expect(dialog.getByText(/starting with a letter/i)).toHaveCount(0);
  // ...but is once the field is left, and the valid one is accepted.
  await name.blur();
  const error = dialog.getByText(/starting with a letter/i);
  await expect(error).toBeVisible();
  expect((await settled(error)).height).toBeGreaterThan(5);

  await name.fill("worker");
  await dialog.getByLabel("Command").fill("echo hello && sleep 3600");
  await dialog.getByRole("button", { name: "Add Process Type" }).click();

  // Anchored: the web row's container name contains "worker" (the application is
  // "Worker Host"), so a bare substring would match both rows.
  const workerRow = page.getByRole("listitem").filter({ hasText: /^worker/ });
  await expect(workerRow).toBeVisible();
  // A worker with nothing set resolves to none, never the application's http.
  await expect(workerRow.getByText("None (default)")).toBeVisible();
  await expect(workerRow.getByText(`${app.slug}-worker-1`)).toBeVisible();
  const pill = workerRow.getByText("Pending", { exact: true });
  await expect(pill).toBeVisible();
  expect((await settled(pill)).width).toBeGreaterThan(20);
  // The worker, unlike web, can be removed.
  await expect(
    workerRow.getByRole("button", { name: "Remove worker" }),
  ).toBeVisible();
});
