import { expect, test, type Page } from "@playwright/test";
import { settled } from "./support/settled";

/**
 * The CrowdSec tab's summary and history, in a real browser.
 *
 * An operator reads this tab during an incident, and jsdom would pass
 * `toBeVisible()` on a card composited at opacity 0 (v0.1.16 shipped exactly that
 * for a toast). So every tile and the history card are checked to be settled at
 * opacity 1 with a real box, not merely present.
 *
 * What the history holds depends on what ran before on this stack (the smoke
 * drill bans and lifts an address), so it does not assert an empty list. When
 * CrowdSec is on it places a ban of its own through the API and checks the row
 * is attributed to a person and not to Belune; the ban is lifted at the end.
 *
 * Environment prepared by scripts/smoke-browser.sh.
 */
async function api<T>(
  page: Page,
  method: string,
  path: string,
  body?: unknown,
): Promise<T | null> {
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
      return res.status === 204 ? null : res.json();
    },
    { method, path, body },
  ) as Promise<T | null>;
}

test("the summary and the history are legible, and a ban is attributed to a person", async ({
  page,
}) => {
  await page.goto("/proxy?tab=crowdsec");

  for (const label of [
    "Bouncer",
    "Bans in force",
    "Automatic bans",
    "Last check",
  ]) {
    const tile = page.locator("dt", { hasText: label });
    await expect(tile).toBeVisible();
    const box = await settled(tile);
    expect(box.width).toBeGreaterThan(0);
    expect(box.height).toBeGreaterThan(0);
    // The value under it is what is read, so it has to be on screen too.
    const value = tile.locator("xpath=following-sibling::dd[1]");
    await expect(value).not.toBeEmpty();
    expect((await settled(value)).height).toBeGreaterThan(0);
  }

  const history = page.getByText("History", { exact: true }).first();
  await expect(history).toBeVisible();
  await settled(history);

  const status = await api<{
    enabled: boolean;
    local_api: { reachable: boolean };
  }>(page, "GET", "/maintenance/proxy/crowdsec");
  // Which branch ran has to reach the job log: the card-only branch asserts far
  // less, and a green tick alone cannot tell the two apart. If CrowdSec stops
  // being on at this point, the attribution would quietly stop being proved.
  const strong = Boolean(status?.enabled && status.local_api.reachable);
  test.info().annotations.push({
    type: "branch",
    description: strong
      ? "placed a ban and asserted attribution"
      : "CrowdSec off: asserted the card only",
  });
  console.log(
    `[crowdsec-overview] branch=${strong ? "attribution" : "card-only"}`,
  );
  if (!strong) {
    // Nothing can be banned: the card still has to say something.
    await expect(
      page
        .getByText("Nothing yet.")
        .or(page.getByRole("columnheader", { name: "When" })),
    ).toBeVisible();
    return;
  }

  // A documentation-range address, so it is bannable and nothing real is.
  const target = `203.0.113.${100 + (Date.now() % 100)}`;
  await api(page, "POST", "/maintenance/proxy/crowdsec/decisions", {
    target,
    duration: "5m",
    reason: "e2e history",
  });
  try {
    await page.reload();
    const row = page.getByRole("row").filter({ hasText: target }).filter({
      hasText: "Banned",
    });
    await expect(row.first()).toBeVisible();
    await settled(row.first());
    // The By cell names someone, and that someone is not Belune: a manual ban
    // must not read as an automatic one, and must not be blank.
    const by = row.first().getByRole("cell").nth(1);
    await expect(by).not.toBeEmpty();
    await expect(by).not.toContainText("Belune (automatic)");
  } finally {
    await api(
      page,
      "DELETE",
      `/maintenance/proxy/crowdsec/decisions?target=${target}`,
    );
  }
});
