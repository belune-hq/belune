import { expect, test } from "@playwright/test";
import { settled } from "./support/settled";
import type { CAPIEnrollment } from "../src/lib/api/capi";

const sharing = {
  share_manual_decisions: false,
  share_custom: true,
  share_tainted: true,
  share_context: false,
};
const path = "**/api/maintenance/proxy/crowdsec/capi";

test("console enrollment is gated on enabled, healthy community protection", async ({
  page,
}) => {
  let enabled = false;
  let health = "healthy";
  let actions = 0;
  await page.route(`${path}/**`, (route) => {
    actions++;
    return route.fulfill({
      status: 400,
      json: { error: "must not be called" },
    });
  });
  await page.route(path, (route) =>
    route.fulfill({
      json: {
        state: enabled ? "enabled" : "not_registered",
        busy: false,
        phase: "idle",
        health,
        sharing_available: true,
        sharing,
      },
    }),
  );
  await page.goto("/proxy?tab=crowdsec");
  const primary = page.getByRole("button", {
    name: "Enroll with approval link",
    exact: true,
  });
  await expect(primary).toBeDisabled();
  await expect(
    page.getByRole("button", { name: "Check enrollment", exact: true }),
  ).toBeDisabled();
  enabled = true;
  health = "unhealthy";
  await page.reload();
  await expect(primary).toBeDisabled();
  expect(actions).toBe(0);
});

test("console enrollment requires consent, waits for approval and restarts only on request", async ({
  page,
}, testInfo) => {
  let record: CAPIEnrollment = {
    state: "not_enrolled",
    restart_required: false,
  };
  const enrollments: Record<string, unknown>[] = [];
  const restarts: unknown[] = [];
  let checks = 0;
  let approved = false;
  await page.route(path, async (route) => {
    if (route.request().method() === "PUT") {
      restarts.push(route.request().postDataJSON());
      record = { ...record, restart_required: false };
      await route.fulfill({ json: { status: "queued" } });
    } else
      await route.fulfill({
        json: {
          state: "enabled",
          busy: false,
          phase: "idle",
          health: "healthy",
          sharing_available: true,
          sharing,
          enrollment: record,
        },
      });
  });
  await page.route(`${path}/enroll`, async (route) => {
    enrollments.push(route.request().postDataJSON());
    record = {
      state: "awaiting_approval",
      restart_required: true,
      requested_at: new Date().toISOString(),
      reason: "Accept the enrollment in CrowdSec",
    };
    const result =
      enrollments.length === 2
        ? {
            ...record,
            outcome: "already_enrolled",
            reason:
              "Already enrolled. Check enrollment or use a key with overwrite.",
          }
        : {
            ...record,
            url:
              enrollments.length === 1
                ? "https://app.crowdsec.net/enroll/browser-fixture"
                : undefined,
          };
    record = { ...result, url: undefined } as CAPIEnrollment;
    await route.fulfill({ json: result });
  });
  await page.route(`${path}/enrollment/check`, async (route) => {
    checks++;
    if (approved)
      record = {
        ...record,
        state: "enrolled",
        outcome: undefined,
        checked_at: new Date().toISOString(),
        reason: "CrowdSec confirmed enrollment",
      };
    await route.fulfill({ json: record });
  });
  await page.goto("/proxy?tab=crowdsec");
  await page
    .getByRole("button", { name: "Enroll with approval link", exact: true })
    .click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("attacked usernames");
  await expect(dialog).toContainText("Manual decisions: Off");
  await expect(dialog).toContainText(
    "Alert context (including attacked usernames): Off",
  );
  const submit = dialog.getByRole("button", {
    name: "Request console enrollment",
    exact: true,
  });
  await expect(submit).toBeDisabled();
  expect(enrollments).toHaveLength(0);
  await dialog
    .getByRole("checkbox", {
      name: "I agree to link this instance and share the data listed above",
      exact: true,
    })
    .check();
  const box = await settled(submit);
  expect(box.width).toBeGreaterThan(100);
  expect(box.height).toBeGreaterThan(20);
  await submit.click();
  await expect(dialog).toHaveCount(0);
  await expect(
    page.getByText("Awaiting approval", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("link", {
      name: "Open CrowdSec approval link",
      exact: true,
    }),
  ).toHaveAttribute("href", "https://app.crowdsec.net/enroll/browser-fixture");
  expect(enrollments).toEqual([
    { mode: "quick", overwrite: false, consent: true, sharing },
  ]);
  expect(restarts).toHaveLength(0);
  expect(checks).toBe(0);
  await page.screenshot({
    path: testInfo.outputPath("console-enrollment.png"),
    fullPage: true,
  });
  const check = page.getByRole("button", {
    name: "Check enrollment",
    exact: true,
  });
  await check.click();
  await expect.poll(() => checks).toBe(1);
  await expect(
    page.getByText("Awaiting approval", { exact: true }),
  ).toBeVisible();
  record = {
    state: "not_enrolled",
    restart_required: false,
    reason: "Central API registration changed",
  };
  await check.click();
  await expect(
    page.getByText("Enrollment not confirmed", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("link", {
      name: "Open CrowdSec approval link",
      exact: true,
    }),
  ).toHaveCount(0);
  await page.reload();
  await expect(
    page.getByText("Enrollment not confirmed", { exact: true }),
  ).toBeVisible();
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  expect(checks).toBe(2);
  await page
    .getByRole("button", { name: "Enroll with approval link", exact: true })
    .click();
  await dialog
    .getByRole("checkbox", {
      name: "I agree to link this instance and share the data listed above",
      exact: true,
    })
    .check();
  await submit.click();
  await expect(
    page.getByText("Already enrolled", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Use enrollment key", exact: true })
    .click();
  await expect(
    dialog.getByLabel("Enrollment key", { exact: true }),
  ).toHaveValue("");
  const overwrite = dialog.getByRole("checkbox", {
    name: "Replace the existing console enrollment",
    exact: true,
  });
  await expect(overwrite).not.toBeChecked();
  await dialog
    .getByLabel("Enrollment key", { exact: true })
    .fill("browser-only-one-shot-key");
  await overwrite.check();
  await dialog
    .getByRole("checkbox", {
      name: "I agree to link this instance and share the data listed above",
      exact: true,
    })
    .check();
  await submit.click();
  await expect.poll(() => enrollments.length).toBe(3);
  expect(enrollments[2]).toEqual({
    mode: "key",
    key: "browser-only-one-shot-key",
    overwrite: true,
    consent: true,
    sharing,
  });
  expect(restarts).toHaveLength(0);
  await page
    .getByRole("button", {
      name: "I accepted enrollment — restart CrowdSec",
      exact: true,
    })
    .click();
  await expect.poll(() => restarts.length).toBe(1);
  expect(restarts[0]).toEqual({ restart_console: true });
  approved = true;
  await check.click();
  await expect(page.getByText("Enrolled", { exact: true })).toBeVisible();
  expect(checks).toBe(3);
});
