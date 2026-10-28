import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { WafExclusion, WafExclusionsPage } from "@/lib/types";

const remove = vi.fn();
let page: WafExclusionsPage | undefined;
let error: Error | null = null;

vi.mock("@/lib/hooks/use-waf", () => ({
  useWafExclusions: () => ({
    data: page,
    isLoading: page === undefined && !error,
    error,
  }),
  useDeleteWafExclusion: () => ({ mutateAsync: remove, isPending: false }),
}));

import { WafExclusionsCard } from "./waf-exclusions-card";

const ex = (over: Partial<WafExclusion> = {}): WafExclusion => ({
  id: 1,
  rule_id: 942290,
  shape: "target",
  collection: "REQUEST_COOKIES",
  target_name: "ph_x",
  path_pattern: "",
  scope: "install",
  application_id: null,
  application_name: "",
  hosts: [],
  inert: false,
  note: "",
  created_at: "2026-10-28T00:00:00Z",
  ...over,
});

beforeEach(() => {
  remove.mockReset();
  remove.mockResolvedValue(undefined);
  page = { exclusions: [], max: 100, collections: ["ARGS"] };
  error = null;
});

describe("WafExclusionsCard", () => {
  it("says what an empty list means, and that silence is not cleanliness", () => {
    render(<WafExclusionsCard />);
    expect(
      screen.getByText(
        /None\. The WAF reports everything the rule set matches/,
      ),
    ).toBeTruthy();
    expect(
      screen.getByText(/a quiet rule here is not a clean application/),
    ).toBeTruthy();
  });

  it("lists what each exclusion ignores, where, and how many of the ceiling", () => {
    page = {
      exclusions: [
        ex(),
        ex({
          id: 2,
          rule_id: 930130,
          shape: "path",
          collection: "",
          target_name: "",
          path_pattern: "/api/*",
          scope: "application",
          application_name: "Shop",
          application_id: "a",
          hosts: ["shop.example.com"],
        }),
      ],
      max: 100,
      collections: [],
    };
    render(<WafExclusionsCard />);
    expect(screen.getByText("2 of 100")).toBeTruthy();
    expect(screen.getByText("REQUEST_COOKIES:ph_x")).toBeTruthy();
    expect(screen.getByText("paths beginning /api/")).toBeTruthy();
    expect(screen.getByText("Every application")).toBeTruthy();
    expect(screen.getByText("Shop")).toBeTruthy();
  });

  it("marks turning a rule off, shows its reason in full, and says when no reason was given", () => {
    page = {
      exclusions: [
        ex({
          id: 3,
          rule_id: 941100,
          shape: "rule",
          collection: "",
          target_name: "",
          note: "our editor posts markup",
        }),
        ex({ id: 4, note: "" }),
      ],
      max: 100,
      collections: [],
    };
    render(<WafExclusionsCard />);
    const whole = screen.getByText("the whole rule");
    expect(whole.className).toContain("text-status-building");
    expect(screen.getByText("our editor posts markup").className).toContain(
      "text-status-building",
    );
    expect(screen.getByText("No reason given")).toBeTruthy();
  });

  it("shows an exclusion that applies to nothing as such, rather than hiding it", () => {
    page = {
      exclusions: [
        ex({
          scope: "application",
          application_name: "Shop",
          application_id: "a",
          inert: true,
        }),
      ],
      max: 100,
      collections: [],
    };
    render(<WafExclusionsCard />);
    const badge = screen.getByText("Shop · no domains");
    expect(badge.getAttribute("title")).toMatch(/applies to nothing right now/);
  });

  it("removes one without a confirmation, because that only puts a rule back in force", async () => {
    page = { exclusions: [ex({ id: 7 })], max: 100, collections: [] };
    render(<WafExclusionsCard />);
    fireEvent.click(screen.getByRole("button", { name: "Remove exclusion" }));
    await waitFor(() => expect(remove).toHaveBeenCalledWith(7));
  });

  it("reports a failed read instead of an empty list that looks clean", () => {
    page = undefined;
    error = new Error("boom");
    render(<WafExclusionsCard />);
    expect(
      screen.getByText(/Could not read the exclusions: boom/),
    ).toBeTruthy();
    expect(screen.queryByText(/None\./)).toBeNull();
  });
});
