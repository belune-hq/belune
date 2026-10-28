import { defineConfig, devices } from "@playwright/test";
import { AUTH_FILE } from "./e2e/support/auth";

/**
 * Playwright drives the REAL production stack, not a dev server.
 *
 * There is deliberately no `webServer` here. The stack under test is the one
 * scripts/smoke-prod.sh boots — the real image, served by the Go binary as a
 * non-root user in a container, behind the real Caddy — because every defect
 * these drills exist to catch was invisible in development. A Vite dev server
 * would serve different bytes (unminified, unbundled, with HMR attached) from a
 * different origin, against an API on the host.
 *
 * So the stack is booted and seeded outside Playwright, and
 * scripts/smoke-browser.sh is the entry point that does both. Run that, not
 * `playwright test`.
 *
 * Reached directly on Belune's own port rather than through Caddy: an app or
 * dashboard hostname force-redirects HTTP→HTTPS (correct for a login form), and
 * every defect here is in the bundle Belune itself serves, which is exactly
 * what this port returns.
 */
export default defineConfig({
  testDir: "./e2e",
  // The request-budget drill holds each page open for a full rate-limit window,
  // so it takes about ten minutes — too long to sit in the prod-smoke job on
  // every push and pull request. Run it deliberately with BELUNE_E2E_BUDGET=1.
  testIgnore: process.env.BELUNE_E2E_BUDGET
    ? []
    : ["**/request-budget.spec.ts"],
  // One shared stack, and the drills mutate install-wide settings. Parallel
  // workers would be editing each other's install.
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  // No retries, in CI either. A rendering defect that appears on one run in two
  // is still a rendering defect, and a retry would turn it into a green tick —
  // which is the failure mode this whole drill exists to end.
  retries: 0,
  // An update journey waits on two server-side polls and a registry round-trip.
  timeout: 150_000,
  expect: {
    // 5s is the default and is too tight for a container stack on a shared CI
    // runner; the long waits are bounded per-assertion in the specs anyway.
    timeout: 15_000,
  },
  reporter: process.env.CI ? [["github"], ["list"]] : [["list"]],
  use: {
    baseURL: process.env.BELUNE_E2E_URL ?? "http://127.0.0.1:18081",
    // Kept only for failures: a trace of a passing run is megabytes of nothing.
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [
    // Signs in once; see auth.setup.ts for why the specs do not each do it.
    { name: "setup", testMatch: /auth\.setup\.ts/ },
    // Chromium alone, on purpose. These drills assert on application behaviour
    // and on computed style, not on cross-browser rendering, so a second engine
    // would triple the install and the runtime to re-answer the same question.
    {
      name: "chromium",
      dependencies: ["setup"],
      use: { ...devices["Desktop Chrome"], storageState: AUTH_FILE },
    },
  ],
});
