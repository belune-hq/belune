import { expect, test, type Page } from "@playwright/test";
import { rendered, settled } from "./support/settled";

async function api<T>(
  page: Page,
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  return page.evaluate(
    async ({ method, path, body }) => {
      const csrf = document.cookie.match(/(?:^|;\s*)csrf_token=([^;]+)/)?.[1];
      const response = await fetch(`/api${path}`, {
        method,
        headers: {
          "Content-Type": "application/json",
          ...(csrf ? { "X-CSRF-Token": csrf } : {}),
        },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (!response.ok)
        throw new Error(`${method} ${path}: ${response.status}`);
      return response.status === 204 ? null : response.json();
    },
    { method, path, body },
  ) as Promise<T>;
}

test("legacy Git links preserve Sources tabs and OAuth outcomes", async ({
  page,
}) => {
  await page.goto("/git?tab=providers&connected=github");
  await expect(page).toHaveURL(/\/sources\?tab=providers$/);
  await expect(
    page.getByText("GitHub App created", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("tab", { name: "Providers", exact: true }),
  ).toHaveAttribute("aria-selected", "true");
  await page.goto("/git?tab=registries");
  await expect(page).toHaveURL(/\/sources\?tab=registries$/);
  await expect(
    page.getByRole("button", { name: "Add registry", exact: true }),
  ).toBeVisible();
  await page.goto("/git?error=gitlab");
  await expect(page).toHaveURL(/\/sources$/);
  await expect(
    page.getByText("Failed to connect GitLab", { exact: true }),
  ).toBeVisible();
});

test("a member sees Connections only and can save a write-only app credential", async ({
  page,
  browser,
}) => {
  await page.goto("/projects");
  const stamp = Date.now();
  const email = `registry-${stamp}@belune.invalid`;
  // A fresh login is necessary to exercise the member's actual authorization.
  const password = "Belune-e2e-member-test-only";
  await api(page, "POST", "/users", {
    email,
    password,
    username: `registry-${stamp}`,
    role: "member",
  });
  const host = `registry-${stamp}.invalid`;
  const global = await api<{ id: string }>(
    page,
    "POST",
    "/settings/registries",
    {
      registry_host: host,
      username: "install-wide-user",
      password: "fake-global-token-for-browser-only",
    },
  );
  const context = await browser.newContext({
    storageState: { cookies: [], origins: [] },
  });
  const member = await context.newPage();
  let project: { id: string } | undefined;
  try {
    await member.goto("/login");
    await member.locator("#email").fill(email);
    await member.locator("#password").fill(password);
    await member.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(member).toHaveURL(/\/projects/);
    await member.goto("/sources?tab=registries");
    await expect(
      member.getByRole("heading", { name: "Sources", exact: true }),
    ).toBeVisible();
    await expect(member.getByRole("tab")).toHaveCount(0);
    await expect(
      member.getByRole("button", { name: "Add registry", exact: true }),
    ).toHaveCount(0);
    expect(
      await member.evaluate(
        async () => (await fetch("/api/settings/registries")).status,
      ),
    ).toBe(403);
    project = await api<{ id: string }>(member, "POST", "/projects", {
      name: `Registry ${stamp}`,
      slug: `registry-${stamp}`,
    });
    const app = await api<{ id: string }>(
      member,
      "POST",
      `/projects/${project.id}/applications`,
      {
        name: "Private image",
        type: "image",
        build_type: "image",
        source_image: `${host}/example/private:latest`,
      },
    );
    const endpoint = `/projects/${project.id}/applications/${app.id}`;
    await member.goto(`${endpoint}/settings`);
    const username = member.getByLabel("Registry username", { exact: true });
    const token = member.getByLabel("Registry password or token", {
      exact: true,
    });
    await expect(token).toBeVisible();
    const coverage = member.getByText(
      `${host} is covered by an install-wide credential.`,
      { exact: true },
    );
    await expect(coverage).toBeVisible();
    await settled(token);
    const box = await rendered(token);
    expect(box.opacity).toBe(1);
    expect(box.width).toBeGreaterThan(100);
    expect(box.height).toBeGreaterThan(20);
    await username.fill("member-registry-user");
    await token.fill("fake-app-token-for-browser-only");
    const save = async () => {
      const response = member.waitForResponse(
        (r) =>
          r.url().endsWith(`/api${endpoint}`) && r.request().method() === "PUT",
      );
      await member
        .getByRole("button", { name: "Save", exact: true })
        .first()
        .click();
      const result = await response;
      expect(result.status()).toBe(200);
      return result.json();
    };
    const saved = await save();
    expect(saved.has_registry_credentials).toBe(true);
    expect(saved).not.toHaveProperty("registry_password");
    expect(saved).not.toHaveProperty("registry_password_encrypted");
    await expect(token).toHaveValue("");
    await member.reload();
    await expect(username).toHaveValue("member-registry-user");
    await expect(token).toHaveValue("");
    await expect(
      member.getByText("Password set", { exact: true }),
    ).toBeVisible();
    expect((await save()).has_registry_credentials).toBe(true);
    await member
      .getByRole("checkbox", {
        name: "Remove application credential on save",
        exact: true,
      })
      .check();
    expect((await save()).has_registry_credentials).toBe(false);
    await member.reload();
    await expect(username).toHaveValue("");
    await expect(coverage).toBeVisible();
    await expect(member.getByText("Password set", { exact: true })).toHaveCount(
      0,
    );
  } finally {
    if (project) await api(member, "DELETE", `/projects/${project.id}`);
    await context.close();
    await api(page, "DELETE", `/settings/registries/${global.id}`);
  }
});
