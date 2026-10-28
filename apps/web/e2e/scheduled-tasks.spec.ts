import { expect, test, type Page } from "@playwright/test";
import { settled } from "./support/settled";

/**
 * The Scheduled Tasks tab, in a real browser.
 *
 * Everything here is something jsdom cannot answer. The cron description and
 * the validation messages are asserted for *visibility* and effective opacity,
 * not merely presence — v0.1.16 shipped a toast composited at opacity 0 twice.
 * The tooltip is the sharper case: it needs hover, focus and layout, none of
 * which exist in jsdom, so the Vitest suite can only prove the markup is there.
 *
 * Environment prepared by scripts/smoke-browser.sh.
 */
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

test("the Schedule field describes, validates without nagging, and hides the raw cron in a tooltip", async ({
  page,
}) => {
  // Already signed in: auth.setup.ts does it once for the whole run.
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/projects/);

  const stamp = Date.now();
  const project = await api<{ id: string }>(page, "POST", "/projects", {
    name: `Tasks ${stamp}`,
    slug: `tasks-${stamp}`,
  });
  const app = await api<{ id: string }>(
    page,
    "POST",
    `/projects/${project.id}/applications`,
    {
      name: "Cron Host",
      type: "image",
      build_type: "image",
      source_image: "nginx:latest",
    },
  );

  await page.goto(`/projects/${project.id}/applications/${app.id}/tasks`);

  // ── The form opens and the description is really on screen ────────────────
  await page.getByRole("button", { name: /new task|add task/i }).click();

  // Scope to the dialog and match the ROLE: getByLabel does substring matching,
  // and the dialog's own accessible name ("Add Scheduled Task") contains
  // "Schedule", so a bare getByLabel("Schedule") is ambiguous.
  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();

  const cron = dialog.getByRole("textbox", { name: "Schedule", exact: true });
  await expect(cron).toBeVisible();
  const box = await settled(cron);
  expect(box.width).toBeGreaterThan(50);
  expect(box.height).toBeGreaterThan(15);

  // cronstrue's wording for a shape the old hand-rolled describer could only
  // echo back verbatim.
  await cron.fill("0 3 * * 1-5");
  const described = dialog.getByText("At 03:00, Monday through Friday");
  await expect(described).toBeVisible();
  await settled(described);

  // ── Typing a valid cron must never flash an error ─────────────────────────
  // isTouched flips on the FIRST KEYSTROKE, so the old gate failed here. A
  // synchronous isVisible() immediately after each keystroke is the only check
  // that can prove a message never appeared: toBeHidden() auto-waits and so
  // passes for one that flashes and clears.
  await cron.fill("");
  for (const partial of ["0", "0 3", "0 3 *", "0 3 * *", "0 3 * * *"]) {
    await cron.fill(partial);
    const flashed = await page
      .getByText(/not a valid cron|5-field cron expression/i)
      .first()
      .isVisible();
    expect(flashed, `an error flashed while typing "${partial}"`).toBe(false);
  }

  // ── An out-of-range expression is flagged once the field is left ──────────
  await cron.fill("0 99 * * *");
  const stillQuiet = await page
    .getByText(/not a valid cron/i)
    .first()
    .isVisible();
  expect(stillQuiet, "flagged before blur").toBe(false);

  await cron.blur();
  const error = dialog.getByText(/not a valid cron/i).first();
  await expect(error).toBeVisible();
  await settled(error);

  // ── Save it, then the list row shows "[schedule] ([timezone])" ────────────
  await cron.fill("0 3 * * 1-5");
  await dialog
    .getByRole("textbox", { name: "Name", exact: true })
    .fill("Nightly report");
  await dialog
    .getByRole("textbox", { name: "Command", exact: true })
    .fill("echo hi");
  await dialog.getByRole("button", { name: /^(save|create)$/i }).click();
  await expect(dialog).toBeHidden();

  const row = page.getByText(/At 03:00, Monday through Friday \(/).first();
  await expect(row).toBeVisible();
  await settled(row);

  // The raw expression must NOT be on the page as text any more...
  expect(
    await page.getByText("0 3 * * 1-5", { exact: true }).first().isVisible(),
  ).toBe(false);

  // ...but hovering the description must reveal it. Hover and layout are the
  // whole point of this assertion; jsdom has neither.
  await row.hover();
  const tip = page.getByText("0 3 * * 1-5", { exact: true }).first();
  await expect(tip).toBeVisible();
  const tipBox = await settled(tip);
  expect(tipBox.width).toBeGreaterThan(10);
  expect(tipBox.height).toBeGreaterThan(5);
});
