import { spawnSync } from "node:child_process";
import { expect, test, type Page } from "@playwright/test";
import { settled } from "./support/settled";

/**
 * Per-instance operation, in a real browser: the Logs tab's process-type selector
 * and the Processes tab's per-container Restart control.
 *
 * What jsdom cannot answer is whether a Select actually renders and can be used —
 * a control composited at opacity 0 passes every jsdom assertion.
 *
 * Log rows are seeded with SQL (scripts/smoke-browser.sh hands this spec
 * BELUNE_E2E_PSQL): the collector only writes them from real containers, and this
 * stack deploys none.
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

function psql(sql: string) {
  const cmd = process.env.BELUNE_E2E_PSQL;
  if (!cmd)
    throw new Error(
      "BELUNE_E2E_PSQL is not set (run scripts/smoke-browser.sh)",
    );
  const res = spawnSync("sh", ["-c", cmd], { input: sql, encoding: "utf8" });
  if (res.status !== 0) throw new Error(`psql failed: ${res.stderr}`);
}

test("the Logs tab filters by process type, and a container has its own Restart control", async ({
  page,
}) => {
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/projects/);

  const stamp = Date.now();
  const project = await api<{ id: string }>(page, "POST", "/projects", {
    name: `Instances ${stamp}`,
    slug: `instances-${stamp}`,
  });
  const app = await api<{ id: string; slug: string }>(
    page,
    "POST",
    `/projects/${project.id}/applications`,
    {
      name: "Two Types",
      type: "image",
      build_type: "image",
      source_image: "nginx:latest",
    },
  );
  await api(
    page,
    "POST",
    `/projects/${project.id}/applications/${app.id}/process-types`,
    {
      name: "worker",
      command: "sleep 3600",
    },
  );

  // Two containers of the worker type (a previous generation and the current one)
  // plus the web container: the filter must reach both worker generations.
  psql(`
    INSERT INTO container_logs (source_type, source_id, level, stream, message, recorded_at, container_id, process_type) VALUES
      ('application', '${app.id}', 'info', 'stdout', 'web-line-alpha',     now() - interval '3 min', 'ctr-web',     'web'),
      ('application', '${app.id}', 'info', 'stdout', 'worker-line-old',    now() - interval '2 min', 'ctr-worker-1', 'worker'),
      ('application', '${app.id}', 'info', 'stdout', 'worker-line-new',    now() - interval '1 min', 'ctr-worker-2', 'worker');
  `);

  await page.goto(`/projects/${project.id}/applications/${app.id}/logs`);
  await expect(page.getByText("web-line-alpha")).toBeVisible();
  await expect(page.getByText("worker-line-old")).toBeVisible();
  await expect(page.getByText("worker-line-new")).toBeVisible();

  // The selector is really on screen, then usable.
  const trigger = page.getByRole("combobox", { name: "Process type" });
  await expect(trigger).toBeVisible();
  expect((await settled(trigger)).width).toBeGreaterThan(40);
  await trigger.click();
  const option = page.getByRole("option", { name: "worker" });
  await expect(option).toBeVisible();
  await option.click();

  // Both worker generations, and nothing from web.
  await expect(page.getByText("worker-line-old")).toBeVisible();
  await expect(page.getByText("worker-line-new")).toBeVisible();
  await expect(page.getByText("web-line-alpha")).toHaveCount(0);

  // The Processes tab: each container has a Restart control. This app was never
  // deployed, so its containers do not exist yet and the control is disabled.
  await page.goto(`/projects/${project.id}/applications/${app.id}/processes`);
  const restart = page.getByRole("button", {
    name: `Restart ${app.slug}-worker-1`,
  });
  await expect(restart).toBeVisible();
  await expect(restart).toBeDisabled();
});
