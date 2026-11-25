import { expect, test as setup } from "@playwright/test";
import { ADOPTION_AUTH_FILE, AUTH_FILE } from "./support/auth";
import {
  createFixtureAdmin,
  signInFixtureAdmin,
} from "./support/fixture-admin";

/**
 * Signs in once per project actor; specs reuse their project's session.
 *
 * Not a convenience. POST /api/auth/login is rate limited per client address (5
 * a minute), and this suite used to sign in once per spec. It got away with it
 * only because the limit's bucket used to be chosen by the client: chi's RealIP
 * rewrote RemoteAddr from X-Forwarded-For, so varying one header bought a fresh
 * bucket. Keyed on the real peer, which is what the limit was always meant to
 * do, seven logins in forty seconds is a 429, and a spec that is not about
 * logging in fails on the login page.
 *
 * Do not make the limit go away for the drill (a flag, a higher number): it
 * would switch off the control that was just made effective, to suit a test.
 * A spec that genuinely has to sign in fresh may, and should say why.
 *
 * The session is cookies (access, refresh, CSRF), which is what storageState
 * keeps, and the access token outlasts the run, so no spec refreshes it.
 */
function required(name: string): string {
  const v = process.env[name];
  if (!v)
    throw new Error(`${name} must be set (scripts/smoke-browser.sh sets it)`);
  return v;
}

setup(
  "sign in browser project admins",
  async ({ page, browser, baseURL }, testInfo) => {
    if (!baseURL) throw new Error("loopback baseURL is required");
    await page.goto("/login");
    await page.locator("#email").fill(required("BELUNE_E2E_EMAIL"));
    await page.locator("#password").fill(required("BELUNE_E2E_PASSWORD"));
    const login = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === "/api/auth/login" &&
        response.request().method() === "POST",
    );
    await page.getByRole("button", { name: "Sign in" }).click();
    const response = await login;
    expect(response.status()).toBe(200);
    const setupBudget = {
      origin: baseURL,
      status: response.status(),
      remaining: response.headers()["x-ratelimit-remaining"],
    };
    console.log("Setup login budget:", setupBudget);
    await testInfo.attach("setup-login-budget", {
      body: JSON.stringify(setupBudget),
      contentType: "application/json",
    });
    await expect(page).toHaveURL(/\/projects/);
    await page.context().storageState({ path: AUTH_FILE });

    // Unconditional: adoption also runs when the HTTP project is not configured.
    const actor = await createFixtureAdmin(page.context(), baseURL, "adoption");
    const context = await browser.newContext({
      baseURL,
      storageState: { cookies: [], origins: [] },
    });
    try {
      const adoption = await context.newPage();
      const budget = await signInFixtureAdmin(adoption, actor, baseURL);
      await testInfo.attach("adoption-login-budget", {
        body: JSON.stringify(budget),
        contentType: "application/json",
      });
      await context.storageState({ path: ADOPTION_AUTH_FILE });
    } finally {
      await context.close();
    }
  },
);
