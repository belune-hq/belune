import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { TrustedProxies } from "@/lib/types";

const save = vi.fn();
let data: TrustedProxies | undefined;
let error: Error | null = null;

vi.mock("@/lib/hooks/use-trusted-proxies", () => ({
  useTrustedProxies: () => ({
    data,
    isLoading: data === undefined && !error,
    error,
  }),
  useSetTrustedProxies: () => ({ mutateAsync: save, isPending: false }),
}));

import { TrustedProxiesCard } from "./trusted-proxies-card";

const cloudflare = {
  id: "cloudflare",
  name: "Cloudflare",
  cidrs: ["173.245.48.0/20", "2606:4700::/32"],
  source: "https://www.cloudflare.com/ips/",
  checked_at: "2026-10-28",
};

function stored(over: Partial<TrustedProxies> = {}): TrustedProxies {
  return {
    cidrs: [],
    overlaps: [],
    overlaps_checked: true,
    presets: [cloudflare],
    ...over,
  };
}

const box = () =>
  screen.getByLabelText(/addresses or ranges/i) as HTMLTextAreaElement;
const saveButton = () => screen.getByRole("button", { name: "Save" });

beforeEach(() => {
  save.mockReset();
  save.mockImplementation((cidrs: string[]) =>
    Promise.resolve(stored({ cidrs })),
  );
  data = stored();
  error = null;
});

describe("TrustedProxiesCard", () => {
  it("always says the list is a trust boundary, before anything is typed", () => {
    render(<TrustedProxiesCard />);
    expect(
      screen.getByText(/every address listed here can claim to be anyone/i),
    ).toBeTruthy();
    expect(
      screen.getByText(/never a range your own apps run in/i),
    ).toBeTruthy();
  });

  it("says what an empty list means and starts with nothing to save", () => {
    render(<TrustedProxiesCard />);
    expect(screen.getByText(/address that connects to Caddy/i)).toBeTruthy();
    expect(box().value).toBe("");
    expect((saveButton() as HTMLButtonElement).disabled).toBe(true);
  });

  it("shows the stored list one range per line and counts it", () => {
    data = stored({ cidrs: ["10.0.0.0/8", "192.168.0.0/16"] });
    render(<TrustedProxiesCard />);
    expect(box().value).toBe("10.0.0.0/8\n192.168.0.0/16");
    expect(screen.getByText("2 ranges trusted.")).toBeTruthy();
  });

  it("labels the preset as a dated snapshot with its source, and adds without saving", () => {
    render(<TrustedProxiesCard />);
    expect(screen.getByText(/a snapshot copied on 2026-10-28/i)).toBeTruthy();
    const link = screen.getByRole("link", { name: /www\.cloudflare\.com/ });
    expect(link.getAttribute("href")).toBe("https://www.cloudflare.com/ips/");
    expect(link.getAttribute("rel")).toContain("noreferrer");
    expect(screen.getByText(/does not keep it current/i)).toBeTruthy();

    fireEvent.click(
      screen.getByRole("button", { name: /add cloudflare ranges/i }),
    );
    expect(box().value).toBe("173.245.48.0/20\n2606:4700::/32");
    expect(save).not.toHaveBeenCalled();
    expect((saveButton() as HTMLButtonElement).disabled).toBe(false);
  });

  it("keeps what was already typed when a preset is added", () => {
    render(<TrustedProxiesCard />);
    fireEvent.change(box(), { target: { value: "10.0.0.0/8" } });
    fireEvent.click(
      screen.getByRole("button", { name: /add cloudflare ranges/i }),
    );
    expect(box().value).toBe("10.0.0.0/8\n173.245.48.0/20\n2606:4700::/32");
  });

  it("saves the parsed list, tolerating commas and spaces", async () => {
    render(<TrustedProxiesCard />);
    fireEvent.change(box(), {
      target: { value: "10.0.0.0/8, 192.168.0.0/16\n10.0.0.0/8" },
    });
    fireEvent.click(saveButton());
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    expect(save).toHaveBeenCalledWith(["10.0.0.0/8", "192.168.0.0/16"]);
  });

  it("waits for the operator to finish before complaining, and never submits a bad list", async () => {
    render(<TrustedProxiesCard />);
    fireEvent.change(box(), { target: { value: "0.0.0.0/0" } });
    // Mid-typing: no error yet, even though the value is already wrong.
    expect(screen.queryByText(/too wide/i)).toBeNull();

    fireEvent.blur(box());
    expect(await screen.findByText(/too wide/i)).toBeTruthy();

    fireEvent.click(saveButton());
    await waitFor(() => expect(screen.getByText(/too wide/i)).toBeTruthy());
    expect(save).not.toHaveBeenCalled();
  });

  it("explains an overlap where the list is edited, naming the range and the network", () => {
    data = stored({
      cidrs: ["172.16.0.0/12"],
      overlaps: [
        {
          cidr: "172.16.0.0/12",
          network: "belune-proj-a",
          subnet: "172.18.0.0/16",
        },
      ],
    });
    render(<TrustedProxiesCard />);
    const alerts = screen.getAllByRole("alert");
    const overlap = alerts.find((a) =>
      /cover networks|covers a network/i.test(a.textContent ?? ""),
    );
    expect(overlap).toBeDefined();
    expect(overlap!.textContent).toMatch(
      /172\.16\.0\.0\/12 covers belune-proj-a \(172\.18\.0\.0\/16\)/,
    );
    expect(overlap!.textContent).toMatch(/claim to be any visitor/);
  });

  it("does not raise an overlap alarm when there is none", () => {
    data = stored({ cidrs: ["10.0.0.0/8"] });
    render(<TrustedProxiesCard />);
    expect(screen.queryByText(/network your apps run on/i)).toBeNull();
    // Only the standing warning.
    expect(screen.getAllByRole("alert")).toHaveLength(1);
  });

  it("says when it could not check, rather than implying there is no overlap", () => {
    data = stored({ cidrs: ["10.0.0.0/8"], overlaps_checked: false });
    render(<TrustedProxiesCard />);
    expect(
      screen.getByText(/could not check whether a range here overlaps/i),
    ).toBeTruthy();
    expect(screen.getAllByRole("alert")).toHaveLength(1);
  });

  it("reports a failed read instead of showing an empty list that looks safe", () => {
    data = undefined;
    error = new Error("boom");
    render(<TrustedProxiesCard />);
    expect(
      screen.getByText(/could not read the trusted proxies: boom/i),
    ).toBeTruthy();
    expect(screen.queryByLabelText(/addresses or ranges/i)).toBeNull();
  });
});
