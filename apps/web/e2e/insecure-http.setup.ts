import { expect, test as setup } from "@playwright/test";
import { HTTP_AUTH_FILE } from "./support/auth";

function required(name: string): string {
  const value = process.env[name];
  if (!value)
    throw new Error(`${name} must be set (scripts/smoke-browser.sh sets it)`);
  return value;
}

// This origin must receive and send the product's own session cookies. Copying
// a loopback session here would hide failures in plain-HTTP login or persistence.
setup("sign in on the plain-HTTP origin", async ({ page, baseURL }) => {
  await page.goto("/login");
  await page.locator("#email").fill(required("BELUNE_E2E_EMAIL"));
  await page.locator("#password").fill(required("BELUNE_E2E_PASSWORD"));
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page).toHaveURL(`${baseURL}/projects`);
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "Projects", exact: true }),
  ).toBeVisible();
  await expect(page).toHaveURL(`${baseURL}/projects`);
  await page.context().storageState({ path: HTTP_AUTH_FILE });
});
