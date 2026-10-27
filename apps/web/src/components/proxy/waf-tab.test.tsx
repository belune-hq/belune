import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import type { WafClientGroup, WafEvent, WafStatus } from "@/lib/types";

/** Comfortably past the filter's 300ms debounce. */
const FILTER_DEBOUNCE_WINDOW_MS = 500;

const setEnabled = vi.fn();
let status: WafStatus | undefined;
let events: WafEvent[] = [];
let clients: WafClientGroup[] = [];
let hasMore = false;
const eventFilters: unknown[] = [];
let hasNextPage = false;
const fetchNextPage = vi.fn();
const clientFilters: unknown[] = [];
const windows: string[] = [];

vi.mock("@/lib/hooks/use-waf", () => ({
  useWafStatus: (window: string) => {
    windows.push(window);
    return { data: status, isLoading: status === undefined };
  },
  useSetWafEnabled: () => ({ mutateAsync: setEnabled, isPending: false }),
  useWafClients: (window: string, filters: unknown) => {
    clientFilters.push({ window, ...(filters as object) });
    return {
      data: { window, clients, has_more: hasMore },
      isLoading: false,
    };
  },
  useWafEvents: (filters: unknown) => {
    eventFilters.push(filters);
    return {
      data: { pages: [{ events, next_before: null }] },
      isLoading: false,
      hasNextPage,
      isFetchingNextPage: false,
      fetchNextPage,
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
  // Absent by default: that is what most detections look like, since a request
  // log is swept on its own retention while the detection stays.
  request: null,
  ...over,
});

const group = (over: Partial<WafClientGroup> = {}): WafClientGroup => ({
  client_ip: "203.0.113.9",
  events: 12,
  requests: 3,
  rules: 4,
  urls: 2,
  last_seen: new Date().toISOString(),
  would_block: false,
  ...over,
});

/** Rendered text and values only — layout is the browser drill's job. */
describe("WafTab", () => {
  beforeEach(() => {
    setEnabled.mockReset().mockResolvedValue(base());
    status = base();
    events = [];
    clients = [];
    hasMore = false;
    hasNextPage = false;
    fetchNextPage.mockReset();
    eventFilters.length = 0;
    clientFilters.length = 0;
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

  it("collapses a client's detections into one row of counts", () => {
    clients = [group({ events: 12, requests: 3, urls: 2, rules: 4 })];
    render(<WafTab />);
    const row = screen.getByText("203.0.113.9").closest("tr")!;
    const cells = within(row).getAllByRole("cell");
    // Expander, client, hits, requests, URLs, rules, last seen.
    expect(cells).toHaveLength(7);
    expect(cells[2].textContent).toBe("12");
    expect(cells[3].textContent).toBe("3");
    expect(cells[4].textContent).toBe("2");
    expect(cells[5].textContent).toBe("4");
  });

  // Hits alone read as twelve attacks; beside Requests they read as one request
  // the rules had twelve opinions about. Both columns are the point.
  it("keeps hits and requests as separate columns", () => {
    clients = [group({ events: 12, requests: 1 })];
    render(<WafTab />);
    expect(screen.getByText(/hits outnumber requests/)).toBeTruthy();
  });

  // The one column that separates an attack from rule noise.
  it("flags a client whose request would have been blocked", () => {
    clients = [
      group({ would_block: true }),
      group({ client_ip: "198.51.100.7" }),
    ];
    render(<WafTab />);
    const flagged = screen.getByText("203.0.113.9").closest("tr")!;
    expect(within(flagged).getByText("Would block")).toBeTruthy();
    const quiet = screen.getByText("198.51.100.7").closest("tr")!;
    expect(within(quiet).queryByText("Would block")).toBeNull();
  });

  // A bare "0" beside three requests reads as a bug in the page. The URL is read
  // from the request log, which has its own retention, so the gap is named.
  it("says why a client has no URLs rather than showing a zero", () => {
    clients = [group({ urls: 0, requests: 3 })];
    render(<WafTab />);
    const cell = screen.getByText("—");
    expect(cell.getAttribute("title")).toContain("No URL on record");
    expect(cell.getAttribute("title")).toContain("retention");
  });

  it("asks nothing for a client's detections until the row is expanded", async () => {
    clients = [group()];
    events = [event()];
    render(<WafTab />);
    expect(eventFilters).toHaveLength(0);

    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );
    await waitFor(() => expect(eventFilters).toHaveLength(1));
    expect(eventFilters.at(-1)).toMatchObject({
      clientIp: "203.0.113.9",
      window: "24h",
    });
    expect(
      screen.getByRole("button", { name: "Hide detections from 203.0.113.9" }),
    ).toBeTruthy();
  });

  it("lists the rules behind an expanded client, with the request each matched", async () => {
    clients = [group()];
    events = [
      event({
        rule_id: 920420,
        request: {
          method: "POST",
          path: "/checkout?step=2",
          status: 500,
          content_type: "text/plain",
          user_agent: "curl/8.7.1",
        },
      }),
    ];
    render(<WafTab />);
    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );

    await waitFor(() => expect(screen.getByText("Severity")).toBeTruthy());
    const panel = screen.getAllByRole("table")[1];
    // The method and status come from the request log, the host from the
    // detection, and they read as one line.
    expect(within(panel).getByText("POST")).toBeTruthy();
    expect(within(panel).getByText("500")).toBeTruthy();
    expect(within(panel).getByText("shop.example.com")).toBeTruthy();
    // The path is deliberately not here: it is the field that runs past any
    // column width, so the detail panel owns it.
    expect(within(panel).queryByText(/checkout/)).toBeNull();
    expect(
      screen.getByText("XSS Attack Detected via libinjection"),
    ).toBeTruthy();
  });

  it("gives the rule's class its own column, left of the rule", async () => {
    clients = [group()];
    events = [
      event({ id: 1, category: "attack-xss" }),
      event({ id: 2, rule_id: 920420, category: "attack-protocol" }),
      // The anomaly summary declares no class, and an empty cell in a column
      // reads as missing data in a way an absent inline badge never did.
      event({ id: 3, rule_id: 949110, category: null, severity: "unknown" }),
    ];
    render(<WafTab />);
    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );

    await waitFor(() => expect(screen.getByText("Category")).toBeTruthy());
    const panel = screen.getAllByRole("table")[1];
    const headers = within(panel)
      .getAllByRole("columnheader")
      .map((h) => h.textContent);
    expect(headers.slice(0, 4)).toEqual([
      "Date",
      "Category",
      "Rule",
      "Severity",
    ]);

    expect(within(panel).getByText("xss")).toBeTruthy();
    expect(within(panel).getByText("protocol")).toBeTruthy();
    // Scoped to the Category cell: these fixtures carry no request either, so
    // the Request column shows its own dash on every row.
    const anomalyRow = within(panel).getByText("949110").closest("tr")!;
    expect(anomalyRow.cells[1].textContent).toBe("—");
    expect(anomalyRow.cells[1].getAttribute("title")).toContain(
      "verdict on the whole request",
    );
  });

  // Severity cannot carry this: 920420 reports `critical` exactly like a SQLi
  // match does, which is what the column exists to separate.
  it("explains the classification on the column, not on every row", async () => {
    clients = [group()];
    events = [event({ rule_id: 920420, category: "attack-protocol" })];
    render(<WafTab />);
    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );

    await waitFor(() => expect(screen.getByText("Category")).toBeTruthy());
    expect(screen.getByText("Category").getAttribute("title")).toContain(
      "autoban acts on the injection classes alone",
    );
    expect(
      screen.getByText("protocol").closest("td")!.getAttribute("title"),
    ).toContain("not an attack payload");
  });

  // "3d" is the right answer in the client row above, which says when that
  // client was last here. A rule match is evidence for one request and gets read
  // against a log somewhere else, so it is dated to the second.
  it("dates a detection to the second", async () => {
    clients = [group()];
    events = [event({ recorded_at: "2026-10-23T14:32:11Z" })];
    render(<WafTab />);
    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );

    await waitFor(() => expect(screen.getByText("Severity")).toBeTruthy());
    // The second table on the page: the first is the client list.
    const panel = screen.getAllByRole("table")[1];
    expect(
      within(panel).getByText(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/),
    ).toBeTruthy();
  });

  it("opens one detection's whole record from its row", async () => {
    clients = [group()];
    events = [
      event({
        rule_id: 920420,
        message: "Request content type is not allowed by policy",
        severity: "warning",
        category: null,
        request: {
          method: "POST",
          // Long enough that the table has to cut it, which is why the panel
          // exists: the query string is the part that decides the detection.
          path: "/search?q=%27%20OR%201%3D1%20--%20&page=2&sort=relevance",
          status: 500,
          content_type: "text/plain",
          user_agent: "curl/8.7.1",
        },
      }),
    ];
    render(<WafTab />);
    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );
    await waitFor(() => expect(screen.getByText("Severity")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Detection details" }));

    const sheet = await waitFor(() => screen.getByRole("dialog"));
    expect(
      within(sheet).getByText("Request content type is not allowed by policy"),
    ).toBeTruthy();
    expect(within(sheet).getByText("Rule 920420")).toBeTruthy();
    expect(within(sheet).getByText("warning")).toBeTruthy();
    // Whole and uncut, including the query.
    expect(
      within(sheet).getByText(
        "/search?q=%27%20OR%201%3D1%20--%20&page=2&sort=relevance",
      ),
    ).toBeTruthy();
    expect(within(sheet).getByText("shop.example.com")).toBeTruthy();
    expect(within(sheet).getByText("text/plain")).toBeTruthy();
    expect(within(sheet).getByText("curl/8.7.1")).toBeTruthy();
  });

  // An operator who reads "Headers" and sees two assumes the request carried
  // two, and then reads a header's absence as evidence about the request.
  it("says the headers are an allowlist, not everything that was sent", async () => {
    clients = [group()];
    events = [
      event({
        request: {
          method: "GET",
          path: "/",
          status: 200,
          content_type: null,
          user_agent: null,
        },
      }),
    ];
    render(<WafTab />);
    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );
    await waitFor(() => expect(screen.getByText("Severity")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Detection details" }));

    const sheet = await waitFor(() => screen.getByRole("dialog"));
    expect(within(sheet).getByText(/two request headers by name/)).toBeTruthy();
    expect(
      within(sheet).getByText("Neither header was present on this request."),
    ).toBeTruthy();
    expect(
      within(sheet).getByText(/matched the rule is not stored/),
    ).toBeTruthy();
  });

  it("the detail panel of a swept detection says so instead of showing a blank URL", async () => {
    clients = [group()];
    events = [event({ request: null })];
    render(<WafTab />);
    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );
    await waitFor(() => expect(screen.getByText("Severity")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Detection details" }));

    const sheet = await waitFor(() => screen.getByRole("dialog"));
    expect(within(sheet).getByText(/No request on record/)).toBeTruthy();
    // And says it once: the URL row above has already said it.
    expect(within(sheet).getAllByText(/No request on record/)).toHaveLength(1);
    expect(
      within(sheet).getByText("They went with the request log."),
    ).toBeTruthy();
  });

  // The host survives a sweep the method and status do not, so the cell still
  // names the application that was targeted instead of going blank.
  it("keeps the host when the request behind a detection is gone", async () => {
    clients = [group()];
    events = [event({ request: null })];
    render(<WafTab />);
    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );

    await waitFor(() => expect(screen.getByText("Severity")).toBeTruthy());
    const cell = screen.getByText("shop.example.com").closest("td")!;
    expect(cell.textContent).toBe("—shop.example.com");
    expect(cell.querySelector("div")!.getAttribute("title")).toContain(
      "retention",
    );
  });

  it("an event with no host is not blank in the panel", async () => {
    clients = [group()];
    events = [event({ host: null })];
    render(<WafTab />);
    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );
    await waitFor(() => expect(screen.getByText("unknown host")).toBeTruthy());
  });

  // Coraza reports no severity for the anomaly-score rule, so the row that says
  // the request would have been blocked used to render as "unknown".
  it("names the anomaly-score row as the verdict, not unknown", async () => {
    clients = [group()];
    events = [
      event({ id: 9, rule_id: 949110, severity: "unknown", category: null }),
    ];
    render(<WafTab />);
    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );
    await waitFor(() => expect(screen.getByText("Would block")).toBeTruthy());
    expect(screen.queryByText("unknown")).toBeNull();
  });

  it("renders the rule id as an identifier, not as a control", async () => {
    clients = [group()];
    events = [event()];
    render(<WafTab />);
    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );
    await waitFor(() => expect(screen.getByText("Severity")).toBeTruthy());

    expect(screen.getByText("941100")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "941100" })).toBeNull();
  });

  // The summary above already says "no detections in the last 24 hours", with
  // the guidance to widen the period. The table must not say it a second time.
  it("does not repeat the summary's empty-period sentence", () => {
    status = base({
      enabled: true,
      state: { active: true, hosts: 1 },
      summary: { ...base().summary, events: 0, total: 19 },
    });
    render(<WafTab />);
    expect(
      screen.getAllByText(/No detections in the last 24 hours/),
    ).toHaveLength(1);
    expect(
      screen.getByText("No client tripped a rule in this period."),
    ).toBeTruthy();
  });

  it("filters the grouped table once typing stops", async () => {
    clients = [group()];
    render(<WafTab />);
    const box = screen.getByLabelText("Filter by client address");

    // Each of these is a complete address, so without the debounce each would
    // have been a request of its own.
    for (const value of ["198.51.100.1", "198.51.100.2", "198.51.100.7"]) {
      fireEvent.change(box, { target: { value } });
    }
    expect(
      clientFilters.filter((f) => (f as { clientIp?: string }).clientIp),
    ).toHaveLength(0);

    await waitFor(() =>
      expect(clientFilters.at(-1)).toMatchObject({ clientIp: "198.51.100.7" }),
    );
    // Only the address typing settled on was ever asked for.
    const asked = clientFilters
      .map((f) => (f as { clientIp?: string }).clientIp)
      .filter(Boolean);
    expect([...new Set(asked)]).toEqual(["198.51.100.7"]);
    expect(screen.getByRole("button", { name: "Clear" })).toBeTruthy();
  });

  it("clears the filter without waiting for the debounce", async () => {
    clients = [group()];
    render(<WafTab />);
    fireEvent.change(screen.getByLabelText("Filter by client address"), {
      target: { value: "198.51.100.7" },
    });
    await waitFor(() =>
      expect(clientFilters.at(-1)).toMatchObject({ clientIp: "198.51.100.7" }),
    );

    fireEvent.click(screen.getByRole("button", { name: "Clear" }));
    expect(
      (screen.getByLabelText("Filter by client address") as HTMLInputElement)
        .value,
    ).toBe("");
    await waitFor(() =>
      expect(clientFilters.at(-1)).toMatchObject({ clientIp: undefined }),
    );
  });

  it("rejects a malformed address before asking the server", async () => {
    clients = [group()];
    render(<WafTab />);
    fireEvent.change(screen.getByLabelText("Filter by client address"), {
      target: { value: "example.com" },
    });
    await waitFor(() =>
      expect(screen.getByText("Enter an IPv4 or IPv6 address")).toBeTruthy(),
    );
    // And it is never sent: half an address matches nothing, so the table holds
    // what it had rather than emptying between the first octet and the last.
    expect(
      clientFilters.filter((f) => (f as { clientIp?: string }).clientIp),
    ).toHaveLength(0);
    expect(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    ).toBeTruthy();
  });

  // The list is capped rather than paged, so it has to say when it is not
  // showing everything — otherwise a thousand-client scan looks like fifty.
  it("shows ten clients at a time and pages through the rest", async () => {
    clients = Array.from({ length: 23 }, (_, i) =>
      group({ client_ip: `203.0.113.${i + 1}`, events: 23 - i }),
    );
    render(<WafTab />);

    expect(
      screen.getAllByRole("button", { name: /^Show detections from/ }),
    ).toHaveLength(10);
    expect(screen.getByText(/1–10 of 23 clients/)).toBeTruthy();
    expect(screen.getByLabelText("Rows per page")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() =>
      expect(screen.getByText(/11–20 of 23 clients/)).toBeTruthy(),
    );
    expect(
      screen.getByRole("button", { name: "Show detections from 203.0.113.11" }),
    ).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() =>
      expect(screen.getByText(/21–23 of 23 clients/)).toBeTruthy(),
    );
    expect(
      screen.getByRole("button", { name: "Next" }).hasAttribute("disabled"),
    ).toBe(true);
  });

  // The filter's debounce used to arm a timer on mount whose only effect was
  // setPage(0), so a page turned inside that window was quietly undone. It
  // failed in CI and passed locally, because the window is 300ms.
  it("does not send the reader back to page one once the debounce elapses", async () => {
    clients = Array.from({ length: 23 }, (_, i) =>
      group({ client_ip: `203.0.113.${i + 1}` }),
    );
    render(<WafTab />);
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() =>
      expect(screen.getByText(/11–20 of 23 clients/)).toBeTruthy(),
    );

    await new Promise((r) => setTimeout(r, FILTER_DEBOUNCE_WINDOW_MS));
    expect(screen.getByText(/11–20 of 23 clients/)).toBeTruthy();
  });

  // Narrowing the list under the cursor must not leave an empty table: the page
  // is clamped rather than reset, so page three of twenty-three becomes the last
  // page of what is left.
  it("does not strand the reader on a page that no longer exists", async () => {
    clients = Array.from({ length: 23 }, (_, i) =>
      group({ client_ip: `203.0.113.${i + 1}` }),
    );
    const { rerender } = render(<WafTab />);
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() =>
      expect(screen.getByText(/21–23 of 23 clients/)).toBeTruthy(),
    );

    clients = clients.slice(0, 4);
    rerender(<WafTab />);
    await waitFor(() =>
      expect(
        screen.getAllByRole("button", { name: /^Show detections from/ }),
      ).toHaveLength(4),
    );
  });

  it("pages a client's detections, and fetches more at the end of them", async () => {
    clients = [group()];
    events = Array.from({ length: 12 }, (_, i) => event({ id: i + 1 }));
    hasNextPage = true;
    render(<WafTab />);
    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );

    await waitFor(() => expect(screen.getByText("Severity")).toBeTruthy());
    const panel = screen.getAllByRole("table")[1];
    expect(within(panel).getAllByRole("row")).toHaveLength(11); // header + 10
    // "+" because the server has more than the twelve loaded.
    expect(screen.getByText(/1–10 of 12\+ detections/)).toBeTruthy();

    const next = screen.getAllByRole("button", { name: "Next" }).at(-1)!;
    fireEvent.click(next);
    await waitFor(() =>
      expect(screen.getByText(/11–12 of 12\+ detections/)).toBeTruthy(),
    );
    expect(fetchNextPage).not.toHaveBeenCalled();

    // Stepping off the end of what is loaded is what asks the server for more,
    // which is the job the "Load more" button used to do.
    fireEvent.click(screen.getAllByRole("button", { name: "Next" }).at(-1)!);
    await waitFor(() => expect(fetchNextPage).toHaveBeenCalledTimes(1));
  });

  it("stops offering more detections when the server has none", async () => {
    clients = [group()];
    events = Array.from({ length: 12 }, (_, i) => event({ id: i + 1 }));
    hasNextPage = false;
    render(<WafTab />);
    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );

    await waitFor(() =>
      expect(screen.getByText(/1–10 of 12 detections/)).toBeTruthy(),
    );
    fireEvent.click(screen.getAllByRole("button", { name: "Next" }).at(-1)!);
    await waitFor(() =>
      expect(screen.getByText(/11–12 of 12 detections/)).toBeTruthy(),
    );
    expect(
      screen
        .getAllByRole("button", { name: "Next" })
        .at(-1)!
        .hasAttribute("disabled"),
    ).toBe(true);
  });

  // A short list has nothing to page and nothing to resize.
  it("shows no pagination when everything fits on one page", async () => {
    clients = [group()];
    events = [event()];
    render(<WafTab />);
    fireEvent.click(
      screen.getByRole("button", { name: "Show detections from 203.0.113.9" }),
    );

    await waitFor(() => expect(screen.getByText("Severity")).toBeTruthy());
    expect(screen.queryByText("Rows per page")).toBeNull();
    expect(screen.queryByRole("button", { name: "Next" })).toBeNull();
  });

  it("says out loud when more clients matched than are listed", () => {
    clients = [group()];
    hasMore = true;
    render(<WafTab />);
    expect(screen.getByText(/More matched/)).toBeTruthy();
  });

  it("explains an empty table by whether it is on", () => {
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

  it("asks the server for the window that was picked, for both tables", async () => {
    status = base({
      summary: { ...base().summary, events: 3, total: 19 },
    });
    clients = [group()];
    render(<WafTab />);
    expect(windows.at(-1)).toBe("24h");
    expect(clientFilters.at(-1)).toMatchObject({ window: "24h" });

    fireEvent.click(screen.getByRole("button", { name: "All kept" }));
    await waitFor(() => expect(windows.at(-1)).toBe("all"));
    expect(clientFilters.at(-1)).toMatchObject({ window: "all" });
  });

  // One list of clients per page: the grouped table below is what the summary's
  // "Top clients" card used to be, over the same period and in the same order.
  it("does not show a second list of clients above the table", () => {
    status = base({
      summary: {
        ...base().summary,
        events: 3,
        total: 19,
        top_clients: [
          {
            client_ip: "203.0.113.9",
            events: 3,
            last_seen: "2026-10-26T00:00:00Z",
          },
        ],
      },
    });
    render(<WafTab />);
    expect(screen.queryByText("Top clients")).toBeNull();
    expect(screen.getByText("Top rules")).toBeTruthy();
  });
});
