import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { SelfUpdateStatus } from "@/lib/api/maintenance";
import { useSelfUpdateStatus, useTriggerSelfUpdate } from "./use-maintenance";

vi.mock("@/lib/api/maintenance", () => ({
  triggerSelfUpdate: vi.fn(),
  getSelfUpdateStatus: vi.fn(),
}));

const maintenanceApi = await import("@/lib/api/maintenance");
const getSelfUpdateStatus = vi.mocked(maintenanceApi.getSelfUpdateStatus);
const triggerSelfUpdate = vi.mocked(maintenanceApi.triggerSelfUpdate);

function wrapper(client: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    );
  };
}

function freshClient() {
  return new QueryClient({
    defaultOptions: {
      // A retry would let a rejected mutation resolve on a later attempt and
      // mask which path the assertion actually took.
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
}

// Both hooks live in one component, which is how update-section.tsx uses them:
// the card polls the status and owns the button that starts the update.
function useUpdateCard() {
  return {
    status: useSelfUpdateStatus(true),
    trigger: useTriggerSelfUpdate(),
  };
}

describe("useTriggerSelfUpdate", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("re-reads the attempt after starting one, so a failure can reach the card", async () => {
    // The defect this covers, live from v0.1.8 to v0.1.16: useSelfUpdateStatus
    // polls only while the state is ALREADY "running", so an update that fails
    // before the first poll left the query holding its mount-time "idle" and
    // the operator saw nothing until they refreshed the page.
    const responses: SelfUpdateStatus[] = [
      { state: "idle" },
      { state: "failed", target: "0.1.99", reason: "manifest has no 0.1.99" },
    ];
    getSelfUpdateStatus.mockImplementation(() =>
      Promise.resolve(responses.shift() ?? { state: "idle" }),
    );
    triggerSelfUpdate.mockResolvedValue({
      status: "accepted",
      target: "0.1.99",
    });

    const client = freshClient();
    const { result } = renderHook(useUpdateCard, {
      wrapper: wrapper(client),
    });

    // The card mounts on the pre-update answer.
    await waitFor(() => expect(result.current.status.data).toBeDefined());
    expect(result.current.status.data?.state).toBe("idle");

    result.current.trigger.mutate({ password: "pw" });

    // The point of the test: the card learns the outcome without a remount or
    // a page reload, because the mutation invalidates the status query.
    await waitFor(() =>
      expect(result.current.status.data?.state).toBe("failed"),
    );
    expect(result.current.status.data?.reason).toBe("manifest has no 0.1.99");
  });

  it("does not re-read the attempt when starting one fails", async () => {
    // A rejected trigger (wrong password, 403, rate limit) must not disturb
    // what the card is showing — onSuccess is the only path that invalidates.
    getSelfUpdateStatus.mockResolvedValue({ state: "idle" });
    triggerSelfUpdate.mockRejectedValue(new Error("invalid password"));

    const client = freshClient();
    const { result } = renderHook(useUpdateCard, {
      wrapper: wrapper(client),
    });

    await waitFor(() => expect(result.current.status.data).toBeDefined());
    const callsBefore = getSelfUpdateStatus.mock.calls.length;

    result.current.trigger.mutate({ password: "wrong" });
    await waitFor(() => expect(result.current.trigger.isError).toBe(true));

    expect(getSelfUpdateStatus.mock.calls.length).toBe(callsBefore);
  });

  it("passes a TOTP code through to the API only when one is given", async () => {
    // The step-up gate is method-agnostic by design, so the hook must not
    // invent a code or drop one.
    getSelfUpdateStatus.mockResolvedValue({ state: "idle" });
    triggerSelfUpdate.mockResolvedValue({ status: "accepted", target: "0.1.17" });

    const client = freshClient();
    const { result } = renderHook(useUpdateCard, {
      wrapper: wrapper(client),
    });

    result.current.trigger.mutate({ password: "pw", code: "123456" });
    await waitFor(() => expect(result.current.trigger.isSuccess).toBe(true));
    expect(triggerSelfUpdate).toHaveBeenCalledWith("pw", "123456");
  });
});

describe("useSelfUpdateStatus", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("does not read the admin-only endpoint when disabled", async () => {
    // Polling this unconditionally would 403 for a Member on every render of
    // the Server page.
    getSelfUpdateStatus.mockResolvedValue({ state: "idle" });

    const client = freshClient();
    renderHook(() => useSelfUpdateStatus(false), { wrapper: wrapper(client) });

    await waitFor(() => expect(getSelfUpdateStatus).not.toHaveBeenCalled());
  });
});
