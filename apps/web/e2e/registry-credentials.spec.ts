import { expect, test } from "@playwright/test";

// Credentials here are fake and never sent to a registry: this exercises the
// real settings API and UI without involving any external account.
test("registry credentials stay write-only through create, edit and remove", async ({
  page,
}) => {
  const host = `registry-${Date.now()}.invalid:5055`;
  await page.goto("/sources?tab=registries");
  await page.getByRole("button", { name: "Add registry", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog
    .getByRole("button", { name: "Save credential", exact: true })
    .click();
  await expect(
    dialog.getByLabel("Registry host", { exact: true }),
  ).toHaveAttribute("aria-invalid", "true");
  await expect(dialog.getByLabel("Password", { exact: true })).toHaveAttribute(
    "aria-invalid",
    "true",
  );
  await dialog.getByLabel("Registry host", { exact: true }).fill(host);
  await dialog.getByLabel("Username", { exact: true }).fill("e2e-user");
  await dialog
    .getByLabel("Password", { exact: true })
    .fill("e2e-fake-password");
  const created = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/settings/registries") &&
      response.request().method() === "POST",
  );
  await dialog
    .getByRole("button", { name: "Save credential", exact: true })
    .click();
  const response = await created;
  expect(response.status()).toBe(201);
  const row = await response.json();
  expect(row.password_set).toBe(true);
  expect(row).not.toHaveProperty("password");
  expect(row).not.toHaveProperty("password_encrypted");
  await expect(dialog).toHaveCount(0);
  await page
    .getByRole("button", { name: `Edit credential for ${host}`, exact: true })
    .click();
  await expect(dialog.getByLabel("Password", { exact: true })).toHaveValue("");
  await dialog.getByLabel("Username", { exact: true }).fill("renamed-e2e-user");
  const updated = page.waitForResponse(
    (response) =>
      response.url().endsWith(`/api/settings/registries/${row.id}`) &&
      response.request().method() === "PUT",
  );
  await dialog
    .getByRole("button", { name: "Save credential", exact: true })
    .click();
  const changed = await (await updated).json();
  expect(changed.username).toBe("renamed-e2e-user");
  expect(changed.password_set).toBe(true);
  expect(changed).not.toHaveProperty("password");
  await expect(dialog).toHaveCount(0);
  await page
    .getByRole("button", { name: `Remove credential for ${host}`, exact: true })
    .click();
  await dialog
    .getByRole("button", { name: "Remove credential", exact: true })
    .click();
  await expect(dialog).toHaveCount(0);
  await expect(
    page.getByRole("button", {
      name: `Edit credential for ${host}`,
      exact: true,
    }),
  ).toHaveCount(0);
});
