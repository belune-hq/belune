import { expect, test } from "@playwright/test";
import {
  installToastProbe,
  maxOpacity,
  TOAST_SAMPLE_MS,
  toastReport,
} from "./support/toast-legibility";

/**
 * The failed-self-update journey, in a real browser against the real production
 * stack.
 *
 * This is the drill that was run by hand three times during v0.1.16, and the
 * three rcs were three laps of it: each one found something the previous could
 * not reach, because nothing in CI could reach any of it. Point
 * `update_latest_version` at a release that does not exist, click Update, and
 * watch what the operator is actually told — the clock while the pull runs, the
 * toast when it fails, and the panel that still says so afterwards.
 *
 * Why a browser and not jsdom: every defect this covers was a RENDERING defect.
 * A toast at `opacity: 0` is in the document with the right text; jsdom has no
 * layout engine and only a partial `getComputedStyle`, so the whole jsdom suite
 * stayed green through all three rcs. See support/toast-legibility.ts.
 *
 * Why a FAILING update and not a successful one: a successful update replaces
 * the container the drill is driving, so there is nothing left to assert
 * against — and a failure is the case that was broken. Nothing on the host is
 * touched either way; the pull is what fails, and it fails before the helper
 * that does the touching exists.
 *
 * The environment is prepared by scripts/smoke-browser.sh, which is also how to
 * run this by hand.
 *
 * ⚠️ Verified by putting the defect back, twice, in a real image — a drill that
 * has never been shown to fail is not evidence of anything. Note that
 * `v0.1.16-rc2` is NOT a usable control: the fix was squash-merged as part of
 * #49, so the pre-squash commit is unreachable from every tag while its content
 * is present in all of them, and the rc2 image passes this drill.
 *
 *   A. the original code, restored verbatim — `toast.success(...)` in onSuccess
 *      and no local announce, so the clock came from the `platform` socket.
 *      → 2 toasts obscured; the clock's text peaked at opacity 0.24.
 *   B. the same duplicate raised BEFORE the announce, so the clock is created
 *      second. The ordering the comment below is about.
 *      → 3 toasts obscured; the clock peaked at 0.25.
 *
 * Both assertions below fire on both; the unmodified image passes.
 */

/** Required rather than defaulted: a default would quietly sign in as nobody
 *  and assert nothing, and the drill's whole job is to not do that. */
function required(name: string): string {
  const value = process.env[name];
  if (!value) {
    throw new Error(
      `${name} is not set — run this drill through scripts/smoke-browser.sh`,
    );
  }
  return value;
}

const PASSWORD = required("BELUNE_E2E_PASSWORD");

/** The release the stack has been told is available. It must not exist in the
 *  registry — that is what makes the pull fail. */
const TARGET = process.env.BELUNE_E2E_TARGET ?? "0.1.99";

/** The failure arrives by poll, not by push: /api/version is re-read every 2s
 *  while a tab is latched on "updating", and the update status every 5s while it
 *  reads "running". Add the registry round-trip for a tag that does not exist
 *  and ~30s is comfortable; the generous bound is so a slow CI runner reports a
 *  real result rather than a timeout. */
const FAILURE_TIMEOUT = 60_000;

test("a self-update that cannot pull its image says so, visibly", async ({
  page,
}) => {
  await installToastProbe(page);

  // Already signed in: auth.setup.ts does it once for the whole run.
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/projects/);

  // Straight to the tab the button lives on. Server → Configuration is also one
  // of the two pages with no WebSocket of its own, which is why the progress
  // announce is dispatched locally rather than awaited from the socket — so
  // this is the page where a regression in that would show.
  await page.goto("/server?tab=configuration");

  // Preconditions, asserted rather than assumed, so a mis-seeded stack fails
  // here with a clear reason instead of further down on a missing button.
  //
  // ⚠️ The card is hidden entirely when the running build reports "dev", since
  // there is nothing meaningful to compare against — so the image under test
  // MUST be stamped with a real VERSION. That is scripts/smoke-prod.sh's
  // --build-arg, and forgetting it looks exactly like "no update available".
  await expect(
    page.getByText(`v${TARGET} is available`),
    `the Updates card is not offering v${TARGET} — is the image stamped with a real VERSION, and update_latest_version seeded?`,
  ).toBeVisible();

  // Unambiguous at this moment: the dialog's own submit button carries the same
  // label, and it does not exist yet.
  await page.getByRole("button", { name: "Update now" }).click();

  const dialog = page.getByRole("dialog");
  await expect(dialog.getByText(`Update to v${TARGET}?`)).toBeVisible();
  // Step-up re-auth: the same password again, which is the gate the host shell
  // uses too. A real one, against real bcrypt — the API rejects anything else,
  // so this half cannot be faked in a unit test.
  await dialog.getByPlaceholder("Password").fill(PASSWORD);
  await dialog.getByRole("button", { name: "Update now" }).click();

  // ⚠️ The reason is deliberately reported TWICE — in the panel and in the
  // toast — so every locator here is scoped to one of them. An unscoped
  // getByText matches both and fails Playwright's strict mode, which reads like
  // a broken test rather than what it is: proof that both halves rendered.
  const dashboard = page.getByRole("main");
  const toasts = page.getByRole("region", { name: /Notifications/ });

  // The durable record. This panel is what is still on screen a minute later,
  // and it was missing entirely from v0.1.8 to v0.1.16 — a failed update was
  // invisible until the operator happened to refresh the page.
  await expect(
    dashboard.getByText(`The last update to v${TARGET} did not complete.`),
  ).toBeVisible({ timeout: FAILURE_TIMEOUT });

  // The reason, not a guess at the reason. v0.1.16 removed a fixed "Does that
  // version exist?" that sent the operator to check a version that was fine,
  // because a pull fails for rate limiting, DNS, a full disk or a registry 5xx
  // just as readily as for a missing tag.
  //
  // ⚠️ Asserted on the "what was being done" half only, deliberately. The cause
  // after the colon is whatever the registry said — manifest unknown for a tag
  // that does not exist, but a DNS failure or a 5xx on an offline runner just as
  // validly. This drill is about what the operator is told, not about ghcr.io
  // being reachable, and that is also why it is not flaky off a network.
  const reason = dashboard.getByText(/Could not pull /);
  await expect(reason).toContainText(
    `Could not pull ghcr.io/weiliang79/belune:${TARGET}`,
  );
  // The reassurance that matters, and the one thing the operator needs before
  // deciding whether to retry: the helper is what touches the host, and it
  // never ran.
  await expect(reason).toContainText("Nothing has changed.");

  // The toast carries the reason too, not just the headline. The panel only
  // reaches someone still on this page — click Update, move to a project, and
  // this is all the operator gets.
  await expect(toasts.getByText(/Could not pull /)).toContainText(
    `Could not pull ghcr.io/weiliang79/belune:${TARGET}`,
  );

  // Now the part only a real browser can answer: was any of this legible?
  //
  // Read from the probe installed before the click, which has been sampling
  // throughout — so none of this races a toast that has already come and gone.
  // The clock lives about 1.7 seconds in a passing run.

  // The failure toast. Polled, because it is still fading in at the moment the
  // panel beside it has finished rendering.
  await expect
    .poll(async () => maxOpacity(await toastReport(page), /^Update to v/), {
      timeout: 15_000,
      message: "the failure toast never became legible",
    })
    .toBeGreaterThan(0.99);

  const report = await toastReport(page);
  const seen = JSON.stringify(report.map((r) => r.title));

  // The elapsed clock was on screen at all. A precondition for everything
  // below, and its own guard: a clock that is never raised and a clock that is
  // raised invisibly look identical to the operator, so they must not look
  // identical here.
  const clock = report.filter((r) => /^Belune is updating/.test(r.title));
  expect(
    clock.reduce((n, r) => n + r.samples, 0),
    `the elapsed clock was never on screen at all. Toasts seen: ${seen}`,
  ).toBeGreaterThan(0);

  // ⚠️ And the one that actually guards v0.1.16's defect: nothing in this
  // journey spends real time stacked behind anything else.
  //
  // Stated over EVERY toast rather than only the clock, because the defect is
  // "two toasts at once" and which of the two ends up hidden is a race — the
  // progress toast was raised by the `platform` socket and the duplicate by the
  // mutation's onSuccess, so whichever arrived second won the front. An
  // assertion scoped to the clock passes outright on the ordering where the
  // clock happens to win, while the bug is just as present.
  //
  // It is also the design this page settled on, not an incidental property: one
  // toast carrying the clock, and the failure toast only once the clock has
  // gone. A change that raises a second toast here is a regression even if it
  // looks harmless, because sonner will hide one of them.
  //
  // The budget is one transition's length, and it is not a fudge factor. A
  // passing run spends exactly one 50ms sample with two toasts live, because
  // the clock's dismissal and the failure toast's arrival land in the same
  // React commit — and the clock's text is still fully opaque through it, since
  // sonner fades content over 400ms. v0.1.16's duplicate sat there for its full
  // 10s duration. There are two orders of magnitude on either side of this
  // line.
  const OBSCURED_BUDGET_MS = 400;
  const obscured = report
    .map((r) => ({ ...r, obscuredMs: r.obscuredSamples * TOAST_SAMPLE_MS }))
    .filter((r) => r.obscuredMs > OBSCURED_BUDGET_MS);
  expect(
    obscured,
    `${obscured.length} toast(s) spent longer than ${OBSCURED_BUDGET_MS}ms stacked behind another, which renders their contents at opacity 0 — something is raising two toasts at once. Toasts seen: ${seen}`,
  ).toEqual([]);

  // A narrow backstop, and it is worth being exact about how narrow.
  //
  // It asserts that the clock's text reached full opacity INSIDE its own toast
  // at some point — so it catches the content being rendered invisible by
  // something within the toast (an opacity on [data-content] or [data-title], a
  // future sonner release hiding a stacked toast by a different mechanism) and,
  // with the sample count above, the clock never being raised at all.
  //
  // ⛔ It does NOT catch v0.1.16's defect, and must not be read as doing so.
  // It is a MAXIMUM over the toast's life, and a toast that is briefly front
  // before a second one pushes it behind reaches full opacity in that window —
  // so the peak is 1 while the operator still could not read it. The assertion
  // above, on `data-front` over time, is the one that catches that; this one is
  // deliberately a different question.
  //
  // ⚠️ Both halves of that were learned the hard way, in the same hour.
  // Measuring opacity ABSOLUTELY (up the whole ancestor chain, including the
  // toast's own 400ms fade) made this assertion flake red on CI against correct
  // code — a fast registry refusal left the clock on screen for under a second,
  // its fade-in never finished, and the peak was 0.851. Measuring it relative
  // to the toast fixes that, and in doing so revealed that its apparent ability
  // to catch the stacking defect had only ever been the element fade capping
  // the peak at 0.24. Proven both ways against a real image with the defect put
  // back: the assertion above fails it, this one does not.
  expect(
    maxOpacity(report, /^Belune is updating/),
    `the elapsed clock's text never reached full opacity inside its toast. Toasts seen: ${seen}`,
  ).toBeGreaterThan(0.99);
});
