import { expect, test } from "@playwright/test";

/**
 * What one dashboard tab costs, measured rather than assumed.
 *
 * The authenticated API is rate limited per CALLER, not per tab
 * (`rateLimitKey` resolves a browser session to `user:<id>`), so every tab of
 * every window a signed-in operator has open spends from one budget. For
 * sixteen releases that budget was 100/min and shared with automated tokens,
 * and a routine v0.1.16 update drill — a couple of tabs, repeated reloads while
 * watching an update — hit 429 on a dashboard doing nothing unusual. Nothing
 * had ever measured what a tab actually spends.
 *
 * ⚠️ Excluded from the default run (see `testIgnore` in playwright.config.ts):
 * it holds each page open for a full window, so it takes about ten minutes.
 * Run it deliberately:
 *
 *     BELUNE_E2E_BUDGET=1 ./scripts/smoke-browser.sh --grep budget
 *
 * It both reports and asserts. The report is the useful half — per-path counts
 * say WHICH query is expensive, which is what a fix needs — but a measurement
 * harness that cannot fail is not a test, so each page also has a ceiling.
 */
const EMAIL = process.env.BELUNE_E2E_EMAIL!;
const PASSWORD = process.env.BELUNE_E2E_PASSWORD!;

const PROJECT = "22222222-2222-2222-2222-222222222222";
const APP = "33333333-3333-3333-3333-333333333333";

/** ⚠️ Keep in step with sessionRateLimit in internal/server/routes.go. Only the
 *  denominator the report prints; the per-page ceiling below is deliberately a
 *  different, fixed number. */
const SESSION_BUDGET = 600;

/** Routes that sit OUTSIDE the rate-limited group — the public group and the
 *  WebSocket group (see internal/server/routes.go). */
const OUTSIDE = [
  "/healthz",
  "/api/version",
  "/api/features",
  "/api/auth/login",
  "/api/auth/setup",
  "/api/auth/refresh",
  "/api/auth/forgot-password",
  "/api/auth/reset-password",
  "/api/auth/invitation",
  "/api/auth/accept-invitation",
  "/api/webhooks/",
  "/api/git/webhooks/",
  "/api/ws",
];

function inBudget(pathname: string): boolean {
  if (!pathname.startsWith("/api/")) return false;
  return !OUTSIDE.some((p) => pathname === p || pathname.startsWith(p));
}

interface Hit {
  t: number;
  path: string;
  method: string;
  counted: boolean;
}

/** Peak requests inside any 60s sliding window — what httprate actually
 *  measures. */
function peak60s(hits: Hit[]): number {
  const counted = hits.filter((h) => h.counted).map((h) => h.t);
  let peak = 0;
  for (const start of counted) {
    const n = counted.filter((t) => t >= start && t < start + 60_000).length;
    peak = Math.max(peak, n);
  }
  return peak;
}

function report(label: string, hits: Hit[], windowMs: number) {
  const counted = hits.filter((h) => h.counted);
  const byPath = new Map<string, number>();
  for (const h of counted) {
    const key =
      h.method +
      " " +
      h.path
        .replace(
          /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/g,
          "{id}",
        )
        .replace(/\/\d+/g, "/{n}");
    byPath.set(key, (byPath.get(key) ?? 0) + 1);
  }
  const rows = [...byPath.entries()].sort((a, b) => b[1] - a[1]);
  console.log(
    `\n=== ${label} — ${(windowMs / 1000).toFixed(0)}s observed ===\n` +
      `counted (in the rate-limited group): ${counted.length}\n` +
      `not counted (public + ws):       ${hits.length - counted.length}\n` +
      `peak in any 60s window:          ${peak60s(hits)}  / ${SESSION_BUDGET}\n` +
      rows.map(([p, n]) => `  ${String(n).padStart(3)}  ${p}`).join("\n"),
  );
}

const WINDOW_MS = Number(process.env.BUDGET_WINDOW_MS ?? 65_000);

const PAGES: [string, string][] = [
  ["/projects", "Projects (landing)"],
  ["/server?tab=configuration", "Server - Configuration"],
  ["/sources?tab=registries", "Sources - Registries"],
  ["/server", "Server - Overview"],
  ["/docker", "Docker"],
  ["/requests", "Requests"],
  ["/certificates", "Certificates"],
  [`/projects/${PROJECT}`, "Project detail"],
  [`/projects/${PROJECT}/applications/${APP}`, "Application detail"],
  [`/projects/${PROJECT}/applications/${APP}/settings`, "Application settings"],
];

// ⚠️ Serial, on ONE page, with ONE sign-in. POST /api/auth/login is rate
// limited to 5/min per IP, so a test-per-page that each signed in would 429 on
// the sixth — and it is also closer to what an operator does: one session,
// navigating.
test.describe.configure({ mode: "serial" });

let page: import("@playwright/test").Page;
const hits: Hit[] = [];
let on = false;

test.beforeAll(async ({ browser }) => {
  page = await browser.newPage();
  page.on("request", (req) => {
    if (!on) return;
    const type = req.resourceType();
    if (type !== "xhr" && type !== "fetch") return;
    const path = new URL(req.url()).pathname;
    if (!path.startsWith("/api/") && path !== "/healthz") return;
    hits.push({
      t: Date.now(),
      path,
      method: req.method(),
      counted: inBudget(path),
    });
  });

  page.on("response", (res) => {
    if (!on) return;
    if (
      res.status() >= 300 &&
      new URL(res.url()).pathname.startsWith("/api/")
    ) {
      console.log(
        `    !! ${res.status()} ${res.request().method()} ${new URL(res.url()).pathname}`,
      );
    }
  });

  await page.goto("/login");
  await page.locator("#email").fill(EMAIL);
  await page.locator("#password").fill(PASSWORD);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page).toHaveURL(/\/projects/);
});

test.afterAll(async () => {
  await page.close();
});

for (const [url, label] of PAGES) {
  test(`budget: ${label}`, async () => {
    hits.length = 0;
    const start = Date.now();
    on = true;
    await page.goto(url);
    await page.waitForTimeout(WINDOW_MS);
    on = false;
    report(label, hits, Date.now() - start);
    const burst = hits.filter((h) => h.counted && h.t - start < 4_000).length;
    console.log(`  mount burst (first 4s): ${burst}`);

    // ⚠️ The ceiling is deliberately the OLD shared budget's two-tab share, not
    // a fraction of the current one. A single tab costing more than this means
    // two tabs would have exhausted the limit that was in place when this was
    // first measured — the shape of the original bug, whatever the number is
    // today. Worst measured page was 33/min before the fixes in this change.
    expect(
      peak60s(hits),
      `${label} spends more than half the old 100/min budget on its own`,
    ).toBeLessThan(50);
  });
}

test("budget: saving application settings", async () => {
  await page.goto(`/projects/${PROJECT}/applications/${APP}/settings`);
  const form = page.locator("form").first();
  const name = form.locator("input").first();
  await expect(name).toBeVisible();
  await page.waitForTimeout(3_000);
  await name.fill(`smoke-${Date.now() % 10000}`);
  // ⚠️ The seeded application is type=image with NO source_image, and
  // validateSource rejects that — so without filling it the PUT 400s, onSuccess
  // never runs, no invalidation happens, and the save looks free. Measured that
  // way twice before noticing.
  await form.getByPlaceholder("nginx:1.27").fill("nginx:1.27");

  hits.length = 0;
  const start = Date.now();
  on = true;
  await form.getByRole("button", { name: "Save", exact: true }).click();
  await page.waitForTimeout(12_000);
  on = false;
  report("one application-settings save (12s)", hits, Date.now() - start);

  // One PUT and the queries its invalidation genuinely touches. Measured at 5
  // (1 PUT + list + detail + deployments + domains), each exactly once — which
  // is the state v0.1.16 restored: query keys are PREFIX-matched, so the extra
  // `detail` invalidation the mutation used to make re-marked four queries that
  // `all` had already covered and fetched each of them a second time. The
  // ceiling catches that coming back.
  const invalidationCost = hits.filter(
    (h) => h.counted && h.t - start < 2_000,
  ).length;
  expect(
    invalidationCost,
    "one save is fetching more than the queries its invalidation covers",
  ).toBeLessThanOrEqual(8);
  for (const h of hits) {
    console.log(
      `    +${String(h.t - start).padStart(5)}ms  ${h.method} ${h.path}`,
    );
  }
});

test("budget: a full page reload", async () => {
  await page.goto("/server?tab=configuration");
  await page.waitForTimeout(3_000);

  hits.length = 0;
  const start = Date.now();
  on = true;
  await page.reload();
  await page.waitForTimeout(6_000);
  on = false;
  report("one reload of Server - Configuration (6s)", hits, Date.now() - start);

  // A reload re-mounts every root query, so this is the largest single burst
  // the dashboard produces and the one an operator repeats while watching an
  // update. Measured at 16.
  expect(
    hits.filter((h) => h.counted).length,
    "a reload costs more than a quarter of the old 100/min budget in one go",
  ).toBeLessThanOrEqual(25);
});
