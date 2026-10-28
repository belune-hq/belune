import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { WafEvent } from "@/lib/types";

const create = vi.fn();

vi.mock("@/lib/hooks/use-waf", () => ({
  useWafExclusions: () => ({
    data: {
      exclusions: [],
      max: 100,
      collections: ["ARGS", "REQUEST_COOKIES", "REQUEST_HEADERS"],
    },
  }),
  useCreateWafExclusion: () => ({ mutateAsync: create, isPending: false }),
}));

import { WafExclusionDialog } from "./waf-exclusion-dialog";

const event = (over: Partial<WafEvent> = {}): WafEvent =>
  ({
    id: 1,
    recorded_at: "2026-10-28T00:00:00Z",
    client_ip: "203.0.113.9",
    rule_id: 942290,
    severity: "critical",
    category: "attack-sqli",
    message: "MongoDB SQL injection",
    host: "shop.example.com",
    application_id: "app-1",
    tx_id: "t",
    request: {
      method: "GET",
      path: "/dashboard?x=1",
      status: 200,
      content_type: null,
      user_agent: null,
    },
    matched_target: "REQUEST_COOKIES:ph_x",
    ...over,
  }) as WafEvent;

const open = (e: WafEvent) =>
  render(<WafExclusionDialog event={e} open onOpenChange={() => {}} />);
const submit = () =>
  fireEvent.click(screen.getByRole("button", { name: "Create Exclusion" }));

beforeEach(() => {
  create.mockReset();
  create.mockResolvedValue({});
});

describe("WafExclusionDialog", () => {
  it("always says it lowers protection, and that the field comes from the request", () => {
    open(event());
    expect(screen.getByText("This lowers your protection")).toBeTruthy();
    expect(screen.getByText(/can be influenced by it/)).toBeTruthy();
  });

  it("starts from the detection's field and its application, and never asks for a field to be typed", async () => {
    open(event());
    expect(screen.getByText("REQUEST_COOKIES:ph_x")).toBeTruthy();
    expect(screen.queryByLabelText(/field name/i)).toBeNull();
    submit();
    await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    expect(create).toHaveBeenCalledWith({
      rule_id: 942290,
      collection: "REQUEST_COOKIES",
      target_name: "ph_x",
      application_id: "app-1",
    });
  });

  it("offers the whole install only as an explicit choice, and warns about it", async () => {
    open(event());
    fireEvent.click(screen.getByRole("button", { name: "Every application" }));
    expect(screen.getByText(/including ones you add later/)).toBeTruthy();
    submit();
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(create.mock.calls[0][0].application_id).toBeUndefined();
  });

  it("explains a keyless match and starts from the path instead", async () => {
    open(event({ matched_target: "REQUEST_FILENAME", rule_id: 930130 }));
    expect(
      screen.getByText(/matched the request as a whole.*no name to exclude/s),
    ).toBeTruthy();
    expect(
      (screen.getByRole("button", { name: "Field" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    expect((screen.getByLabelText("Path") as HTMLInputElement).value).toBe(
      "/dashboard",
    );
    submit();
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(create).toHaveBeenCalledWith({
      rule_id: 930130,
      path_pattern: "/dashboard",
      application_id: "app-1",
    });
  });

  it("does not offer a field whose name could not be written safely, and says why", () => {
    open(event({ matched_target: "ARGS:a,ctl:ruleEngine=Off" }));
    expect(screen.getByText(/cannot be written safely/)).toBeTruthy();
    expect(
      (screen.getByRole("button", { name: "Field" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    expect(screen.queryByText(/ruleEngine/)).toBeNull();
  });

  it("refuses to submit a bad path and says what is wrong", async () => {
    open(event({ matched_target: "REQUEST_FILENAME", rule_id: 930130 }));
    fireEvent.change(screen.getByLabelText("Path"), {
      target: { value: "/*" },
    });
    submit();
    expect(await screen.findByText(/'\/\*' is every path/)).toBeTruthy();
    expect(create).not.toHaveBeenCalled();
  });

  it("needs a reason to turn a rule off, and sends it", async () => {
    open(event({ matched_target: null, request: null }));
    expect(screen.getByText("Why")).toBeTruthy();
    submit();
    expect(
      await screen.findByText(/Say why this rule is being turned off/),
    ).toBeTruthy();
    expect(create).not.toHaveBeenCalled();

    fireEvent.change(screen.getByLabelText("Why"), {
      target: { value: " our editor " },
    });
    await waitFor(() =>
      expect(
        (
          screen.getByRole("button", {
            name: "Create Exclusion",
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(false),
    );
    submit();
    await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    expect(create).toHaveBeenCalledWith({
      rule_id: 942290,
      application_id: "app-1",
      note: "our editor",
    });
  });

  it("cannot scope to an application the detection does not name", () => {
    open(event({ application_id: null }));
    expect(
      (
        screen.getByRole("button", {
          name: "This application",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    expect(screen.getByText(/not tied to an application/)).toBeTruthy();
  });

  it("refuses the verdict rule outright, with the reason, and offers no way to create it", () => {
    open(event({ rule_id: 949110 }));
    expect(screen.getByText("This rule cannot be excluded")).toBeTruthy();
    expect(screen.getByText(/the verdict, not a signature/)).toBeTruthy();
    expect(
      screen.queryByRole("button", { name: "Create Exclusion" }),
    ).toBeNull();
  });
});
