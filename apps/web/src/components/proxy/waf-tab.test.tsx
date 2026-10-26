import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import type { WafEvent, WafStatus } from "@/lib/types";

const setEnabled = vi.fn();
let status: WafStatus | undefined;
let events: WafEvent[] = [];
const eventFilters: unknown[] = [];
const windows: string[] = [];

vi.mock("@/lib/hooks/use-waf", () => ({
  useWafStatus: (window: string) => {
    windows.push(window);
    return { data: status, isLoading: status === undefined };
  },
  useSetWafEnabled: () => ({ mutateAsync: setEnabled, isPending: false }),
  useWafEvents: (filters: unknown) => {
    eventFilters.push(filters);
    return {
      data: { pages: [{ events, next_before: null }] },
      isLoading: false,
      hasNextPage: false,
      isFetchingNextPage: false,
      fetchNextPage: vi.fn(),
    };
  },
}));

import { WafTab } from "./waf-tab";

const base = (over: Partial<WafStatus> = {}): WafStatus => ({
  capability: { module: "http.handlers.waf", state: "available" },
  enabled: false,
  mode: "detection_only",
  state: { active: false, hosts: 0 },
  recorder: {
    since: "2026-10-23T00:00:00Z",
    recorded: 0,
    dropped: 0,
    malformed: 0,
  },
  retention_days: 7,
  max_events: 200000,
  summary: {
    window: "24h",
    window_hours: 24,
    events: 0,
    requests: 0,
    clients: 0,
    total: 0,
    top_rules: [],
    top_clients: [],
  },
  ...over,
});

const event = (over: Partial<WafEvent> = {}): WafEvent => ({
  id: 1,
  recorded_at: new Date().toISOString(),
  client_ip: "203.0.113.9",
  rule_id: 941100,
  severity: "critical",
  category: "attack-xss",
  message: "XSS Attack Detected via libinjection",
  host: "shop.example.com",
  application_id: null,
  tx_id: "tx",
  ...over,
});

/** Rendered text and values only — layout is the browser drill's job. */
describe("WafTab", () => {
  beforeEach(() => {
    setEnabled.mockReset().mockResolvedValue(base());
    status = base();
    events = [];
    eventFilters.length = 0;
    windows.length = 0;
  });

  // The test that matters: if `unknown` rendered as `unavailable`, someone whose
  // Caddy was only restarting would be told their image lacks the module.
  it("tells could-not-ask apart from the module being absent", () => {
    status = base({ capability: { module: "m", state: "unknown" } });
    const { unmount } = render(<WafTab />);
    expect(screen.getByText("Cannot tell yet")).toBeTruthy();
    expect(screen.queryByText(/does not include the WAF module/)).toBeNull();
    expect(screen.getByRole("switch").getAttribute("aria-disabled")).toBeNull();
    unmount();

    status = base({ capability: { module: "m", state: "unavailable" } });
    render(<WafTab />);
    expect(screen.getByText(/does not include the WAF module/)).toBeTruthy();
    expect(screen.getByRole("switch").getAttribute("aria-disabled")).toBe(
      "true",
    );
  });

  it("says what turning it on does, before it is turned on", async () => {
    render(<WafTab />);
    fireEvent.click(screen.getByRole("switch"));

    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(/Nothing is blocked/)).toBeTruthy();
    expect(within(dialog).getByText(/not to this dashboard/)).toBeTruthy();
    expect(within(dialog).getByText(/128 KiB/)).toBeTruthy();
    expect(setEnabled).not.toHaveBeenCalled();

    fireEvent.click(within(dialog).getByRole("button", { name: "Turn on" }));
    await waitFor(() => expect(setEnabled).toHaveBeenCalledWith(true));
  });

  it("cancelling the confirmation leaves it off", async () => {
    render(<WafTab />);
    fireEvent.click(screen.getByRole("switch"));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(setEnabled).not.toHaveBeenCalled();
  });

  it("turning it off needs no confirmation", async () => {
    status = base({ enabled: true, state: { active: true, hosts: 2 } });
    render(<WafTab />);
    expect(screen.getByText("Detecting")).toBeTruthy();
    fireEvent.click(screen.getByRole("switch"));
    await waitFor(() => expect(setEnabled).toHaveBeenCalledWith(false));
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("says out loud when detections were lost", () => {
    status = base({
      enabled: true,
      state: { active: true, hosts: 1 },
      recorder: {
        since: "x",
        recorded: 10,
        dropped: 1200,
        malformed: 2,
        last_error: "connection refused",
      },
    });
    render(<WafTab />);
    expect(
      screen.getByText(/Could not save events: connection refused/),
    ).toBeTruthy();
    expect(screen.getByText(/1,200 events were dropped/)).toBeTruthy();
    expect(screen.getByText(/2 log lines/)).toBeTruthy();
  });

  it("lists detections, and an event with no host is not blank", () => {
    events = [
      event({ id: 2, host: null }),
      event({ id: 1, client_ip: "198.51.100.7" }),
    ];
    render(<WafTab />);
    expect(screen.getByText("unknown host")).toBeTruthy();
    expect(screen.getByText("shop.example.com")).toBeTruthy();
    expect(
      screen.getAllByText("XSS Attack Detected via libinjection"),
    ).toHaveLength(2);
    expect(screen.getAllByText("xss")).toHaveLength(2);
  });

  it("filters to one client from a row", async () => {
    events = [event()];
    render(<WafTab />);
    fireEvent.click(screen.getByRole("button", { name: "203.0.113.9" }));
    await waitFor(() =>
      expect(eventFilters.at(-1)).toEqual({ clientIp: "203.0.113.9" }),
    );
    expect(screen.getByRole("button", { name: "Clear" })).toBeTruthy();
  });

  it("rejects a malformed address before asking the server", async () => {
    render(<WafTab />);
    fireEvent.change(screen.getByLabelText("Filter by client address"), {
      target: { value: "example.com" },
    });
    await waitFor(() =>
      expect(screen.getByText("Enter an IPv4 or IPv6 address")).toBeTruthy(),
    );
  });

  it("explains an empty list by whether it is on", () => {
    const { unmount } = render(<WafTab />);
    expect(screen.getByText(/The WAF is off/)).toBeTruthy();
    unmount();

    status = base({ enabled: true, state: { active: true, hosts: 1 } });
    render(<WafTab />);
    expect(screen.getByText("No detections yet.")).toBeTruthy();
  });

  // The regression this card was rebuilt for. The summary was hardcoded to 24
  // hours and returned null on an empty window, so a day after an attack the
  // whole dashboard disappeared and left a bare table with no totals at all.
  it("stays on screen when the window is empty but events are kept", () => {
    status = base({
      enabled: true,
      state: { active: true, hosts: 1 },
      summary: { ...base().summary, events: 0, total: 19 },
    });
    render(<WafTab />);
    expect(screen.getByText(/No detections in the last 24 hours/)).toBeTruthy();
    expect(screen.getByText(/19 kept in total/)).toBeTruthy();
    expect(screen.getByLabelText("Summary period")).toBeTruthy();
  });

  it("shows nothing above the table when nothing was ever recorded", () => {
    status = base({ enabled: true, state: { active: true, hosts: 1 } });
    render(<WafTab />);
    expect(screen.queryByLabelText("Summary period")).toBeNull();
    expect(screen.getByText("No detections yet.")).toBeTruthy();
  });

  it("asks the server for the window that was picked", async () => {
    status = base({
      summary: { ...base().summary, events: 3, total: 19 },
    });
    render(<WafTab />);
    expect(windows.at(-1)).toBe("24h");

    fireEvent.click(screen.getByRole("button", { name: "All kept" }));
    await waitFor(() => expect(windows.at(-1)).toBe("all"));
  });

  // Coraza reports no severity for the anomaly-score rule, so the row that says
  // the request would have been blocked used to render as "unknown".
  it("names the anomaly-score row as the verdict, not unknown", () => {
    events = [
      event({ id: 9, rule_id: 949110, severity: "unknown", category: null }),
    ];
    render(<WafTab />);
    expect(screen.getByText("Would block")).toBeTruthy();
    expect(screen.queryByText("unknown")).toBeNull();
  });

  it("filters to one rule from a row", async () => {
    events = [event()];
    render(<WafTab />);
    fireEvent.click(screen.getByRole("button", { name: "941100" }));
    await waitFor(() =>
      expect(eventFilters.at(-1)).toEqual({
        clientIp: undefined,
        ruleId: 941100,
      }),
    );
    expect(screen.getByText(/Showing rule/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Clear" })).toBeTruthy();
  });
});
