import { expect, test as setup } from "@playwright/test";
import { AUTH_FILE, HTTP_AUTH_FILE } from "./support/auth";
import {
  createFixtureAdmin,
  signInFixtureAdmin,
} from "./support/fixture-admin";

// This origin must receive and send the product's own session cookies. Copying
// a loopback session here would hide failures in plain-HTTP login or persistence.
setup(
  "sign in on the plain-HTTP origin",
  async ({ page, browser, baseURL }, testInfo) => {
    if (!baseURL) throw new Error("HTTP baseURL is required");
    const loopback = process.env.BELUNE_E2E_URL ?? "http://127.0.0.1:18081";
    const admin = await browser.newContext({
      baseURL: loopback,
      storageState: AUTH_FILE,
    });
    const actor = await createFixtureAdmin(admin, loopback, "http").finally(
      () => admin.close(),
    );
    const budget = await signInFixtureAdmin(page, actor, baseURL);
    await testInfo.attach("http-login-budget", {
      body: JSON.stringify(budget),
      contentType: "application/json",
    });
    await page.reload();
    await expect(
      page.getByRole("heading", { name: "Projects", exact: true }),
    ).toBeVisible();
    await expect(page).toHaveURL(`${baseURL}/projects`);
    await page.context().storageState({ path: HTTP_AUTH_FILE });
  },
);
