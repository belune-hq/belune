import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import type { CrowdSecDecision, CrowdSecStatus } from "@/lib/types";

// vi.mock is hoisted above everything else in the file, so what its factory
// reads has to be hoisted with it.
const h = vi.hoisted(() => {
  const mutation = (fn: ReturnType<typeof vi.fn>) => () => ({
    mutateAsync: fn,
    isPending: false,
  });
  return {
    setEnabled: vi.fn(),
    ban: vi.fn(),
    lift: vi.fn(),
    allow: vi.fn(),
    unallow: vi.fn(),
    saveAutoban: vi.fn(),
    rotate: vi.fn(),
    mutation,
    state: {
      status: undefined as unknown,
      decisions: [] as unknown[],
      allowlist: [] as unknown[],
      wafEnabled: true,
    },
  };
});
const { setEnabled, ban, lift, allow, unallow, saveAutoban, rotate } = h;

vi.mock("@/lib/hooks/use-crowdsec", () => ({
  useCrowdSecStatus: () => ({
    data: h.state.status,
    isLoading: h.state.status === undefined,
  }),
  useCrowdSecDecisions: () => ({
    data: { decisions: h.state.decisions },
    isLoading: false,
    error: null,
  }),
  useCrowdSecAllowlist: () => ({
    data: { items: h.state.allowlist },
    isLoading: false,
    error: null,
  }),
  useSetCrowdSecEnabled: h.mutation(h.setEnabled),
  useBanAddress: h.mutation(h.ban),
  useLiftBan: h.mutation(h.lift),
  useAllowAddress: h.mutation(h.allow),
  useRemoveAllowed: h.mutation(h.unallow),
  useSetCrowdSecAutoban: h.mutation(h.saveAutoban),
  useRotateCrowdSecKey: h.mutation(h.rotate),
}));
vi.mock("@/lib/hooks/use-waf", () => ({
  useWafStatus: () => ({ data: { enabled: h.state.wafEnabled } }),
}));

import { CrowdSecTab } from "./crowdsec-tab";

const base = (over: Partial<CrowdSecStatus> = {}): CrowdSecStatus => ({
  capability: { module: "http.handlers.crowdsec", state: "available" },
  enabled: false,
  state: { active: false, hosts: 0 },
  local_api: {
    checked_at: new Date().toISOString(),
    reachable: true,
    bouncer_registered: true,
    last_pull: new Date().toISOString(),
  },
  your_ip: "203.0.113.50",
  autoban: {
    policy: {
      enabled: false,
      threshold: 10,
      window_minutes: 10,
      duration_minutes: 60,
      max_active: 25,
      categories: ["attack-sqli", "attack-xss"],
    },
    status: { active: 0, banned: 0, skipped: 0, capped: false },
    categories: ["attack-lfi", "attack-sqli", "attack-xss"],
  },
  ...over,
});

const decision = (over: Partial<CrowdSecDecision> = {}): CrowdSecDecision => ({
  id: 1,
  value: "198.51.100.7",
  scope: "ip",
  source: "dashboard",
  reason: "belune: port scanner",
  created_at: new Date().toISOString(),
  expires_at: new Date(Date.now() + 3 * 3600_000 + 60_000).toISOString(),
  ...over,
});

const on = () =>
  base({
    enabled: true,
    state: { active: true, hosts: 2 },
  });

/** Rendered text and values only — layout is the browser drill's job. */
describe("CrowdSecTab", () => {
  beforeEach(() => {
    for (const f of [
      setEnabled,
      ban,
      lift,
      allow,
      unallow,
      saveAutoban,
      rotate,
    ]) {
      f.mockReset().mockResolvedValue(undefined);
    }
    h.state.status = base();
    h.state.decisions = [];
    h.state.allowlist = [];
    h.state.wafEnabled = true;
  });

  // If `unknown` rendered as `unavailable`, someone whose Caddy was only
  // restarting would be told their image lacks the module.
  it("tells could-not-ask apart from the module being absent", () => {
    h.state.status = base({ capability: { module: "m", state: "unknown" } });
    const { unmount } = render(<CrowdSecTab />);
    expect(screen.getByText("Cannot tell yet")).toBeTruthy();
    expect(
      screen.queryByText(/does not include the CrowdSec module/),
    ).toBeNull();
    expect(
      screen.getAllByRole("switch")[0].getAttribute("aria-disabled"),
    ).toBeNull();
    unmount();

    h.state.status = base({
      capability: { module: "m", state: "unavailable" },
    });
    render(<CrowdSecTab />);
    expect(
      screen.getByText(/does not include the CrowdSec module/),
    ).toBeTruthy();
    expect(screen.getAllByRole("switch")[0].getAttribute("aria-disabled")).toBe(
      "true",
    );
  });

  it("says what turning it on does, naming the address, before it does it", async () => {
    render(<CrowdSecTab />);
    fireEvent.click(screen.getByRole("switch", { name: /Enforce bans/ }));

    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(/203\.0\.113\.50/)).toBeTruthy();
    expect(within(dialog).getByText(/never behind it/)).toBeTruthy();
    expect(within(dialog).getByText(/CDN or load balancer/)).toBeTruthy();
    expect(setEnabled).not.toHaveBeenCalled();

    fireEvent.click(within(dialog).getByRole("button", { name: "Turn on" }));
    await waitFor(() => expect(setEnabled).toHaveBeenCalledWith(true));
  });

  it("turning it off needs no confirmation", async () => {
    h.state.status = on();
    render(<CrowdSecTab />);
    fireEvent.click(screen.getByRole("switch", { name: /Enforce bans/ }));
    await waitFor(() => expect(setEnabled).toHaveBeenCalledWith(false));
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("shows the bans and allowlist only when there is a local API to ask", () => {
    h.state.status = base({
      enabled: true,
      state: { active: false, hosts: 0, reason: "lapi_unavailable" },
      local_api: {
        checked_at: "",
        reachable: false,
        bouncer_registered: false,
        error: "not running",
      },
    });
    render(<CrowdSecTab />);
    expect(screen.getByText("Local API down")).toBeTruthy();
    expect(screen.queryByText("Bans")).toBeNull();
    expect(screen.queryByText("Never banned")).toBeNull();
    // The way back is always on the page.
    expect(screen.getByText("If a ban locks you out")).toBeTruthy();
  });

  it("offers to rotate the key exactly when the bouncer is not pulling", () => {
    h.state.status = on();
    const { unmount } = render(<CrowdSecTab />);
    expect(
      screen.queryByRole("button", { name: /Rotate the bouncer key/ }),
    ).toBeNull();
    unmount();

    h.state.status = {
      ...on(),
      local_api: {
        ...on().local_api,
        last_pull: new Date(Date.now() - 600_000).toISOString(),
      },
    };
    render(<CrowdSecTab />);
    expect(screen.getByText("Not enforcing")).toBeTruthy();
    fireEvent.click(
      screen.getByRole("button", { name: /Rotate the bouncer key/ }),
    );
    expect(rotate).toHaveBeenCalled();
  });

  describe("bans", () => {
    it("lists them with where they came from, without Belune's own prefix", () => {
      h.state.status = on();
      h.state.decisions = [
        decision(),
        decision({
          id: 2,
          value: "198.51.100.0/24",
          scope: "range",
          source: "waf",
          reason: "belune-waf: attack-sqli: 14 requests in 10m",
        }),
      ];
      render(<CrowdSecTab />);
      expect(screen.getByText("198.51.100.7")).toBeTruthy();
      expect(screen.getByText("port scanner")).toBeTruthy();
      expect(screen.getByText("Dashboard")).toBeTruthy();
      expect(screen.getByText("WAF policy")).toBeTruthy();
      expect(screen.getByText("range")).toBeTruthy();
      expect(screen.getByText("attack-sqli: 14 requests in 10m")).toBeTruthy();
      expect(screen.getAllByText("3 hours")).toHaveLength(2);
    });

    it("lifts a ban with one click, and also when the feature is off", async () => {
      h.state.status = base({ enabled: false });
      h.state.decisions = [decision()];
      render(<CrowdSecTab />);
      const button = screen.getByRole("button", {
        name: "Lift the ban on 198.51.100.7",
      });
      expect((button as HTMLButtonElement).disabled).toBe(false);
      fireEvent.click(button);
      await waitFor(() => expect(lift).toHaveBeenCalledWith("198.51.100.7"));
    });

    it("refuses an address that is not one, before asking the server", async () => {
      h.state.status = on();
      render(<CrowdSecTab />);
      fireEvent.change(screen.getByLabelText("Address to ban"), {
        target: { value: "not-an-ip" },
      });
      fireEvent.click(screen.getByRole("button", { name: "Ban" }));
      await screen.findByText("Enter an IP address or a CIDR range");
      expect(ban).not.toHaveBeenCalled();
    });

    it("bans what was typed, for a day unless told otherwise", async () => {
      h.state.status = on();
      render(<CrowdSecTab />);
      fireEvent.change(screen.getByLabelText("Address to ban"), {
        target: { value: " 203.0.113.9 " },
      });
      fireEvent.change(screen.getByLabelText("Reason"), {
        target: { value: "scanner" },
      });
      fireEvent.click(screen.getByRole("button", { name: "Ban" }));
      await waitFor(() =>
        expect(ban).toHaveBeenCalledWith({
          target: "203.0.113.9",
          duration: "24h",
          reason: "scanner",
        }),
      );
    });

    it("keeps what was typed when the server refuses", async () => {
      h.state.status = on();
      ban.mockRejectedValue(
        new Error("that would ban the address you are connected from"),
      );
      render(<CrowdSecTab />);
      const input = screen.getByLabelText("Address to ban") as HTMLInputElement;
      fireEvent.change(input, { target: { value: "203.0.113.50" } });
      fireEvent.click(screen.getByRole("button", { name: "Ban" }));
      await waitFor(() => expect(ban).toHaveBeenCalled());
      expect(input.value).toBe("203.0.113.50");
    });

    it("will not ban while it is off, and says why", () => {
      h.state.status = base({ enabled: false });
      render(<CrowdSecTab />);
      expect(
        (screen.getByRole("button", { name: "Ban" }) as HTMLButtonElement)
          .disabled,
      ).toBe(true);
      expect(
        screen.getByText(/Turn CrowdSec on to ban an address/),
      ).toBeTruthy();
    });
  });

  describe("allowlist", () => {
    it("marks the address you are connected from", () => {
      h.state.status = on();
      h.state.allowlist = [
        {
          value: "203.0.113.50",
          description: "login: admin@example.com",
          created_at: "",
          expires_at: new Date(Date.now() + 86400_000 * 20).toISOString(),
        },
        { value: "192.0.2.0/28", description: "office", created_at: "" },
      ];
      render(<CrowdSecTab />);
      expect(screen.getByText("you")).toBeTruthy();
      expect(screen.getByText("login: admin@example.com")).toBeTruthy();
      expect(screen.getByText("never")).toBeTruthy();
    });

    it("adds and removes", async () => {
      h.state.status = on();
      h.state.allowlist = [
        { value: "192.0.2.0/28", description: "office", created_at: "" },
      ];
      render(<CrowdSecTab />);

      fireEvent.change(screen.getByLabelText("Address to allowlist"), {
        target: { value: "198.51.100.4" },
      });
      fireEvent.click(screen.getByRole("button", { name: "Add" }));
      await waitFor(() =>
        expect(allow).toHaveBeenCalledWith({
          target: "198.51.100.4",
          duration: "30d",
          note: undefined,
        }),
      );

      fireEvent.click(
        screen.getByRole("button", {
          name: "Remove 192.0.2.0/28 from the allowlist",
        }),
      );
      await waitFor(() => expect(unallow).toHaveBeenCalledWith("192.0.2.0/28"));
    });
  });

  describe("automatic bans", () => {
    it("is off, and says what it would do", () => {
      render(<CrowdSecTab />);
      expect(
        screen
          .getByRole("checkbox", { name: /Ban automatically/ })
          .getAttribute("aria-checked"),
      ).toBe("false");
      expect(
        screen.getByText(/Bans an address once 10 separate requests/),
      ).toBeTruthy();
      // Only attack categories are on offer.
      expect(screen.queryByRole("checkbox", { name: "protocol" })).toBeNull();
      expect(screen.getByRole("checkbox", { name: "sqli" })).toBeTruthy();
    });

    it("asks before the first time a detection can become a ban", async () => {
      h.state.status = on();
      render(<CrowdSecTab />);
      fireEvent.click(
        screen.getByRole("checkbox", { name: /Ban automatically/ }),
      );
      fireEvent.click(screen.getByRole("button", { name: "Save" }));

      const dialog = await screen.findByRole("alertdialog");
      expect(
        within(dialog).getByText(/real visitor can trip attack rules too/),
      ).toBeTruthy();
      expect(
        within(dialog).getByText(/Every automatic ban expires/),
      ).toBeTruthy();
      expect(saveAutoban).not.toHaveBeenCalled();

      fireEvent.click(within(dialog).getByRole("button", { name: "Turn on" }));
      await waitFor(() =>
        expect(saveAutoban).toHaveBeenCalledWith({
          enabled: true,
          threshold: 10,
          window_minutes: 10,
          duration_minutes: 60,
          max_active: 25,
          categories: ["attack-sqli", "attack-xss"],
        }),
      );
    });

    it("cancelling the question saves nothing", async () => {
      h.state.status = on();
      render(<CrowdSecTab />);
      fireEvent.click(
        screen.getByRole("checkbox", { name: /Ban automatically/ }),
      );
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
      const dialog = await screen.findByRole("alertdialog");
      fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
      expect(saveAutoban).not.toHaveBeenCalled();
    });

    it("tuning an already-on policy saves without asking", async () => {
      const s = on();
      s.autoban.policy.enabled = true;
      h.state.status = s;
      render(<CrowdSecTab />);
      fireEvent.change(screen.getByLabelText("Requests"), {
        target: { value: "30" },
      });
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
      await waitFor(() => expect(saveAutoban).toHaveBeenCalled());
      expect(saveAutoban.mock.calls[0][0].threshold).toBe(30);
      expect(screen.queryByRole("alertdialog")).toBeNull();
    });

    it("will not save a threshold the server would refuse, or no category at all", async () => {
      const s = on();
      s.autoban.policy.enabled = true;
      h.state.status = s;
      render(<CrowdSecTab />);

      fireEvent.change(screen.getByLabelText("Requests"), {
        target: { value: "1" },
      });
      await screen.findByText("Between 3 and 1000");
      expect(
        (screen.getByRole("button", { name: "Save" }) as HTMLButtonElement)
          .disabled,
      ).toBe(true);
      fireEvent.change(screen.getByLabelText("Requests"), {
        target: { value: "10" },
      });

      fireEvent.click(screen.getByRole("checkbox", { name: "sqli" }));
      fireEvent.click(screen.getByRole("checkbox", { name: "xss" }));
      await screen.findByText("Pick at least one category");
      expect(saveAutoban).not.toHaveBeenCalled();
    });

    it("says so when it is on but the WAF that feeds it is off", () => {
      const s = on();
      s.autoban.policy.enabled = true;
      h.state.status = s;
      h.state.wafEnabled = false;
      render(<CrowdSecTab />);
      expect(
        screen.getByText(/The WAF is off, so there are no detections/),
      ).toBeTruthy();
    });
  });

  it("always shows the way back, saying what will not help", () => {
    render(<CrowdSecTab />);
    expect(screen.getByText(/cscli decisions delete --all/)).toBeTruthy();
    expect(
      screen.getByText(/host shell in this dashboard will not help/),
    ).toBeTruthy();
    expect(
      screen.getByText(/UPDATE crowdsec_config SET enabled = false/),
    ).toBeTruthy();
  });
});
