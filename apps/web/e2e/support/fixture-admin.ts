import { randomBytes } from "node:crypto";
import { expect, type BrowserContext, type Page } from "@playwright/test";

// Each later project needs a different user, not another session sharing the
// setup admin's rate-limit bucket. Credentials live only in this setup process.
export async function createFixtureAdmin(
  context: BrowserContext,
  origin: string,
  purpose: "http" | "adoption",
) {
  const username = `e2e-${purpose}-${randomBytes(6).toString("hex")}`;
  const email = `${username}@belune.invalid`;
  const password = randomBytes(24).toString("hex");
  const csrf = (await context.cookies(origin)).find(
    (cookie) => cookie.name === "csrf_token",
  )?.value;
  const response = await context.request.post(
    new URL("/api/users", origin).href,
    {
      headers: csrf ? { "X-CSRF-Token": csrf } : {},
      data: { username, email, password, role: "admin" },
    },
  );
  expect(
    response.status(),
    "create project fixture admin through real API",
  ).toBe(201);
  const user = (await response.json()) as { id: string };
  return { id: user.id, email, password };
}

export async function signInFixtureAdmin(
  page: Page,
  actor: Awaited<ReturnType<typeof createFixtureAdmin>>,
  origin: string,
) {
  await page.goto("/login");
  await page.locator("#email").fill(actor.email);
  await page.locator("#password").fill(actor.password);
  const login = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === "/api/auth/login" &&
      response.request().method() === "POST",
  );
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  const response = await login;
  expect(
    response.status(),
    "fixture login must respect the real IP limit",
  ).toBe(200);
  await expect(page).toHaveURL(`${origin}/projects`);
  const me = await page.evaluate(async () =>
    (await fetch("/api/auth/me")).json(),
  );
  expect(me).toMatchObject({ id: actor.id, email: actor.email, role: "admin" });
  const budget = {
    actorID: actor.id,
    origin,
    status: response.status(),
    remaining: response.headers()["x-ratelimit-remaining"],
  };
  console.log("Fixture login budget:", budget);
  return budget;
}
