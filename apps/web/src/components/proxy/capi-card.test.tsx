import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import type { CAPIStatus } from "@/lib/api/capi";
const h = vi.hoisted(() => ({
  status: undefined as CAPIStatus | undefined,
  change: vi.fn(),
  check: vi.fn(),
}));
vi.mock("@/lib/hooks/use-capi", () => ({
  useCAPIStatus: () => ({ data: h.status, isLoading: false, isError: false }),
  useChangeCAPI: () => ({ mutateAsync: h.change, isPending: false }),
  useCheckCAPIConnection: () => ({ mutateAsync: h.check, isPending: false }),
}));
import { CAPICard } from "./capi-card";
const initial: CAPIStatus = {
  state: "not_registered",
  busy: false,
  phase: "idle",
  health: "healthy",
  sharing_available: true,
  sharing: {
    share_manual_decisions: false,
    share_custom: true,
    share_tainted: true,
    share_context: false,
  },
};
beforeEach(() => {
  h.status = structuredClone(initial);
  h.change.mockReset();
  h.check.mockReset();
  h.change.mockResolvedValue({ status: "queued" });
});
describe("Central API consent and recovery", () => {
  it("requires consent and submits the actual sharing preferences", async () => {
    render(<CAPICard />);
    expect(
      screen
        .getByRole("switch", { name: "Share custom scenario signals" })
        .getAttribute("aria-checked"),
    ).toBe("true");
    expect(
      screen
        .getByRole("switch", { name: "Share modified scenario signals" })
        .getAttribute("aria-checked"),
    ).toBe("true");
    expect(
      screen
        .getByRole("switch", { name: "Share alert context" })
        .getAttribute("aria-checked"),
    ).toBe("false");
    fireEvent.click(
      screen.getByRole("switch", { name: "Share custom scenario signals" }),
    );
    fireEvent.click(screen.getByRole("switch", { name: "Central API" }));
    expect(h.change).not.toHaveBeenCalled();
    const dialog = await screen.findByRole("dialog");
    expect(
      within(dialog).getByText("Share custom scenario signals: Off"),
    ).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(h.change).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("switch", { name: "Central API" }));
    fireEvent.click(
      within(await screen.findByRole("dialog")).getByRole("button", {
        name: "Enable and share signals",
      }),
    );
    await waitFor(() =>
      expect(h.change).toHaveBeenCalledWith({
        enabled: true,
        sharing: { ...initial.sharing, share_custom: false },
      }),
    );
  });
  it("reports verified recovery and allows a retry", () => {
    h.status = {
      ...initial,
      state: "failed_reverted",
      reason: "The change was reverted; the local API is healthy",
    };
    render(<CAPICard />);
    expect(screen.getByText("Local protection restored")).toBeTruthy();
    expect(
      screen
        .getByRole("switch", { name: "Central API" })
        .hasAttribute("disabled"),
    ).toBe(false);
  });
  it("allows disabling when authentication and sharing status are unavailable", async () => {
    h.status = {
      ...initial,
      state: "unavailable",
      health: "unhealthy",
      sharing_available: false,
    };
    render(<CAPICard />);
    expect(screen.queryByRole("switch", { name: "Central API" })).toBeNull();
    fireEvent.click(
      screen.getByRole("button", { name: "Disable Central API" }),
    );
    await waitFor(() =>
      expect(h.change).toHaveBeenCalledWith({ enabled: false }),
    );
  });
  it("blocks duplicate changes while the worker is applying", () => {
    h.status = { ...initial, busy: true, phase: "applying" };
    render(<CAPICard />);
    expect(screen.getByRole("status").textContent).toContain(
      "waiting for CrowdSec",
    );
    for (const toggle of screen.getAllByRole("switch"))
      expect(toggle.getAttribute("aria-disabled")).toBe("true");
  });
  it("checks the connection only after an explicit click", async () => {
    h.status = { ...initial, state: "enabled" };
    render(<CAPICard />);
    expect(h.check).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Check connection" }));
    await waitFor(() => expect(h.check).toHaveBeenCalledTimes(1));
  });
  it("explains the rate limit without claiming local protection failed", () => {
    h.status = {
      ...initial,
      state: "enabled",
      connection: {
        state: "rate_limited",
        reason:
          "CrowdSec is rate-limiting this server's IP; disable Central API and wait about an hour",
      },
    };
    render(<CAPICard />);
    expect(screen.getByText("Central API rate limit")).toBeTruthy();
    expect(screen.getByText(/wait about an hour/)).toBeTruthy();
    expect(h.check).not.toHaveBeenCalled();
  });
});
