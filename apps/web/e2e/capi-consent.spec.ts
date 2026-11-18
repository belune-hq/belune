import { expect, test } from "@playwright/test";

// Rendering/consent coverage uses a controlled API response so the browser
// suite never registers CI machines with an external service. The gated Go
// Docker drill covers real CAPI registration, restart and recovery separately.
test("Central API explains sharing and requires explicit consent", async ({
  page,
}) => {
  const sharing = {
    share_manual_decisions: false,
    share_custom: true,
    share_tainted: true,
    share_context: false,
  };
  const mutations: unknown[] = [];
  await page.route("**/api/maintenance/proxy/crowdsec/capi", async (route) => {
    if (route.request().method() === "PUT") {
      mutations.push(route.request().postDataJSON());
      await route.fulfill({ json: { status: "queued" } });
    } else {
      await route.fulfill({
        json: {
          state: "not_registered",
          busy: false,
          phase: "idle",
          health: "healthy",
          sharing_available: true,
          sharing,
        },
      });
    }
  });
  await page.goto("/proxy?tab=crowdsec");
  const toggle = page.getByRole("switch", { name: "Central API", exact: true });
  await expect(toggle).toBeEnabled();
  await expect(
    page.getByRole("switch", {
      name: "Share custom scenario signals",
      exact: true,
    }),
  ).toBeChecked();
  await page
    .getByRole("switch", { name: "Share custom scenario signals", exact: true })
    .click();
  await toggle.click();
  const dialog = page.getByRole("dialog");
  await expect(
    dialog.getByText("Share custom scenario signals: Off", { exact: true }),
  ).toBeVisible();
  await expect(dialog).toContainText("sends detection signals to CrowdSec");
  expect(mutations).toHaveLength(0);
  const confirm = dialog.getByRole("button", {
    name: "Enable and share signals",
    exact: true,
  });
  const rendered = await confirm.evaluate((element) => {
    const style = getComputedStyle(element);
    return {
      opacity: style.opacity,
      width: element.getBoundingClientRect().width,
    };
  });
  expect(Number(rendered.opacity)).toBeGreaterThan(0);
  expect(rendered.width).toBeGreaterThan(0);
  await confirm.click();
  await expect.poll(() => mutations.length).toBe(1);
  expect(mutations[0]).toEqual({
    enabled: true,
    sharing: { ...sharing, share_custom: false },
  });
  await expect(dialog).toHaveCount(0);
});

test("Central API polls stay local and connection checks require a click", async ({
  page,
}) => {
  let checks = 0;
  const connection = {
    state: "rate_limited",
    reason:
      "CrowdSec's Central API is rate-limiting this server's IP address. Disable Central API and wait about an hour before enabling or checking again; ongoing retries can extend the limit",
    checked_at: new Date().toISOString(),
  };
  await page.route("**/api/maintenance/proxy/crowdsec/capi", (route) =>
    route.fulfill({
      json: {
        state: "enabled",
        busy: false,
        phase: "idle",
        health: "healthy",
        sharing_available: true,
        sharing: {
          share_manual_decisions: false,
          share_custom: true,
          share_tainted: true,
          share_context: false,
        },
      },
    }),
  );
  await page.route("**/api/maintenance/proxy/crowdsec/capi/check", (route) => {
    checks++;
    return route.fulfill({ json: connection });
  });
  await page.goto("/proxy?tab=crowdsec");
  const button = page.getByRole("button", {
    name: "Check connection",
    exact: true,
  });
  await expect(button).toBeEnabled();
  expect(checks).toBe(0);
  await button.click();
  await expect(
    page.getByText("Central API rate limit", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText(connection.reason, { exact: false }),
  ).toBeVisible();
  expect(checks).toBe(1);
});
