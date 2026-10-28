import { expect, test, type Page } from "@playwright/test";
import { rendered, settled } from "./support/settled";

/**
 * A stored route feature the running Caddy cannot apply, in a real browser.
 *
 * The composition is new — a disabled Switch, a "Not active" badge and a line
 * per reason in the dialog, and a dashed amber badge in the table — and jsdom
 * cannot tell us any of it is visible: it passes `toBeVisible()` on an element
 * composited at opacity 0, which is what v0.1.16 shipped. The failure being
 * guarded is a domain that LOOKS rate-limited and is not, so what matters here
 * is what a person sees.
 *
 * The capabilities response is stubbed per scenario. The Go suite proves what
 * the endpoint says for a given Caddy; this drill proves what the UI does with
 * each answer, and the stub keeps it independent of which Caddy image the smoke
 * stack happens to boot. The two "not inert" scenarios are the controls: without
 * them every inert assertion would pass against a page that always says so.
 *
 * Environment prepared by scripts/smoke-browser.sh.
 */
type State = "available" | "unavailable" | "unknown";

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

/** Later routes win, so each scenario simply registers its own answer. */
async function stubCapabilities(page: Page, state: State) {
  await page.route("**/api/proxy/capabilities", (route) =>
    route.fulfill({
      json: {
        known: state !== "unknown",
        features: {
          rate_limit: { module: "http.handlers.rate_limit", state },
        },
      },
    }),
  );
}

test("a rate limit the Caddy cannot apply reads as not active, not as on", async ({
  page,
}) => {
  // Already signed in: auth.setup.ts does it once for the whole run.
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/projects/);

  const stamp = Date.now();
  const project = await api<{ id: string }>(page, "POST", "/projects", {
    name: `Inert ${stamp}`,
    slug: `inert-${stamp}`,
  });
  const app = await api<{ id: string }>(
    page,
    "POST",
    `/projects/${project.id}/applications`,
    {
      name: "Inert App",
      type: "image",
      build_type: "image",
      source_image: "nginx:latest",
    },
  );
  const domain = await api<{ id: string }>(
    page,
    "POST",
    `/projects/${project.id}/applications/${app.id}/domains`,
    { hostname: "limited.inert-drill.invalid", ssl_mode: "off" },
  );
  await api(
    page,
    "PUT",
    `/projects/${project.id}/applications/${app.id}/domains/${domain.id}/features`,
    {
      feature_type: "rate_limit",
      config: { window: "1s", max_events: 10 },
      enabled: true,
    },
  );

  // The real endpoint answers in the shape the stubs mimic; only the state is
  // allowed to differ between Caddys.
  const real = await api<{
    known: boolean;
    features: Record<string, { module: string; state: string }>;
  }>(page, "GET", "/proxy/capabilities");
  expect(real.features.rate_limit.module).toBe("http.handlers.rate_limit");
  expect(["available", "unavailable", "unknown"]).toContain(
    real.features.rate_limit.state,
  );

  const domainsUrl = `/projects/${project.id}/applications/${app.id}/domains`;

  const openFeatures = async () => {
    await page.getByRole("button", { name: "Actions" }).click();
    await page.getByRole("menuitem", { name: "Route features" }).click();
    const dialog = page.getByRole("dialog", { name: "Route Features" });
    await expect(dialog).toBeVisible();
    return dialog;
  };

  for (const state of ["available", "unknown"] as const) {
    await test.step(`${state}: an ordinary, live rate limit`, async () => {
      await stubCapabilities(page, state);
      await page.goto(domainsUrl);

      const badge = page.getByText("Rate Limit", { exact: true });
      await expect(badge).toBeVisible();
      await expect(page.getByText("not active")).toHaveCount(0);

      const dialog = await openFeatures();
      const toggle = dialog.getByRole("switch", { name: "Enable rate_limit" });
      await expect(toggle).toBeEnabled();
      await expect(toggle).toBeChecked();
      await expect(dialog.getByText("Not active")).toHaveCount(0);
      await expect(dialog.getByTestId("feature-inert-module")).toHaveCount(0);
    });
  }

  await test.step("unavailable: visibly not active, and the switch is not ON", async () => {
    await stubCapabilities(page, "unavailable");
    await page.goto(domainsUrl);

    const badge = page.getByText("Rate Limit · not active");
    await expect(badge).toBeVisible();
    const badgeBox = await settled(badge);
    expect(badgeBox.opacity).toBe(1);
    expect(badgeBox.width).toBeGreaterThan(40);
    expect(badgeBox.height).toBeGreaterThan(10);

    const dialog = await openFeatures();

    const toggle = dialog.getByRole("switch", { name: "Enable rate_limit" });
    await expect(toggle).toBeDisabled();
    await expect(toggle).not.toBeChecked();
    expect((await settled(toggle)).opacity).toBeGreaterThan(0.3);

    const chip = dialog.getByText("Not active", { exact: true });
    await expect(chip).toBeVisible();
    const chipBox = await settled(chip);
    expect(chipBox.opacity).toBe(1);
    expect(chipBox.width).toBeGreaterThan(30);
    expect(chipBox.height).toBeGreaterThan(10);

    const reason = dialog.getByTestId("feature-inert-module");
    await expect(reason).toBeVisible();
    await expect(reason).toContainText("http.handlers.rate_limit");
    await expect(reason).toContainText("does not have");
    const reasonBox = await settled(reason);
    expect(reasonBox.opacity).toBe(1);
    expect(reasonBox.height).toBeGreaterThan(8);
    // The explanation is for a person to read: not collapsed to a sliver.
    expect(reasonBox.width).toBeGreaterThan(100);
    expect((await rendered(reason)).opacity).toBe(1);

    // The type stays on offer — disabled, with the reason — rather than vanishing.
    await dialog.getByRole("combobox").click();
    const option = page.getByRole("option", { name: /Rate Limit/ });
    await expect(option).toBeVisible();
    await expect(option).toHaveAttribute("aria-disabled", "true");
    await expect(option).toContainText("does not have");
  });

  // The hole this step closes: a Caddy Belune cannot exec into reads "unknown"
  // for ever, so the capability probe never says "unavailable" and the row above
  // would render a plain ON toggle for a feature the reconciler strips on every
  // pass. The domain list carries what the reconciler OBSERVED instead; the
  // handler only reports it past a grace period, which the Go suite proves, so
  // the list is stubbed here to be past it.
  for (const reason of ["modules_unknown", "module_absent"] as const) {
    await test.step(`${reason}: not active although the probe says unknown`, async () => {
      await stubCapabilities(page, "unknown");
      await page.route(
        /\/api\/projects\/[^/]+\/applications\/[^/]+\/domains(\?.*)?$/,
        async (route) => {
          if (route.request().method() !== "GET") return route.continue();
          const res = await route.fetch();
          const rows = (await res.json()) as Record<string, unknown>[];
          await route.fulfill({
            response: res,
            json: rows.map((d) => ({
              ...d,
              not_applied: [
                {
                  feature_type: "rate_limit",
                  reason,
                  since: "2026-10-20T07:12:04Z",
                },
              ],
            })),
          });
        },
      );
      await page.goto(domainsUrl);

      const badge = page.getByText("Rate Limit · not active");
      await expect(badge).toBeVisible();
      const badgeBox = await settled(badge);
      expect(badgeBox.opacity).toBe(1);
      expect(badgeBox.width).toBeGreaterThan(40);

      const dialog = await openFeatures();
      const toggle = dialog.getByRole("switch", { name: "Enable rate_limit" });
      await expect(toggle).toBeDisabled();
      await expect(toggle).not.toBeChecked();

      const chip = dialog.getByText("Not active", { exact: true });
      await expect(chip).toBeVisible();
      expect((await settled(chip)).opacity).toBe(1);

      const line = dialog.getByTestId("feature-inert-not-applied");
      await expect(line).toBeVisible();
      await expect(line).toContainText(
        reason === "modules_unknown"
          ? "CADDY_CONTAINER_NAME"
          : "Belune Caddy image",
      );
      const lineBox = await settled(line);
      expect(lineBox.opacity).toBe(1);
      expect(lineBox.width).toBeGreaterThan(100);
      expect((await rendered(line)).opacity).toBe(1);
      // The probe said nothing, so the module line must not appear.
      await expect(dialog.getByTestId("feature-inert-module")).toHaveCount(0);
    });
  }
});
