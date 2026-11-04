import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ProcessType } from "@/lib/types";

const createMutate = vi.fn();
let types: ProcessType[] = [];

vi.mock("@/lib/hooks/use-process-types", () => ({
  useProcessTypes: () => ({ data: types, isLoading: false }),
  useCreateProcessType: () => ({
    mutateAsync: createMutate,
    isPending: false,
  }),
  useUpdateProcessType: () => ({ mutateAsync: vi.fn(), isPending: false }),
  useDeleteProcessType: () => ({ mutateAsync: vi.fn(), isPending: false }),
}));
vi.mock("sonner", () => ({
  toast: { promise: vi.fn(), warning: vi.fn(), error: vi.fn() },
}));

import { ProcessTypesSection } from "./process-types-section";

const pt = (over: Partial<ProcessType>): ProcessType => ({
  id: "id",
  name: "web",
  command: "",
  routable: false,
  health_check_type: null,
  health_check_command: null,
  instances: [],
  created_at: "",
  updated_at: "",
  ...over,
});

const renderIt = () =>
  render(
    <ProcessTypesSection
      projectId="p"
      applicationId="a"
      appHealthType="http"
    />,
  );

/**
 * Rendered text and the accessible tree only — jsdom has no layout engine, so
 * whether a person can SEE any of this is the browser drill's job.
 */
describe("ProcessTypesSection", () => {
  beforeEach(() => {
    createMutate.mockReset().mockResolvedValue({});
    types = [
      pt({
        id: "w",
        name: "web",
        routable: true,
        instances: [
          {
            id: "i1",
            idx: 1,
            container_name: "proj-app-1234abcd",
            container_id: "c1",
            status: "running",
          },
        ],
      }),
      pt({
        id: "k",
        name: "worker",
        command: "bundle exec sidekiq",
        instances: [
          {
            id: "i2",
            idx: 1,
            container_name: "proj-app-1234abcd-worker-1",
            container_id: null,
            status: "pending",
          },
        ],
      }),
    ];
  });

  it("offers no remove control for the routable type", () => {
    renderIt();
    expect(screen.queryByRole("button", { name: "Remove web" })).toBeNull();
    expect(screen.getByRole("button", { name: "Remove worker" })).toBeTruthy();
    expect(screen.getByText(/cannot be removed/i)).toBeTruthy();
  });

  it("shows the resolved health check, with a worker's as none", () => {
    renderIt();
    // The routable type inherits the application's http; the worker's NULL check
    // resolves to none, never http.
    expect(screen.getByText("HTTP probe (the application's)")).toBeTruthy();
    expect(screen.getByText("None (default)")).toBeTruthy();
  });

  it("shows each type's container and status", () => {
    renderIt();
    expect(screen.getByText("proj-app-1234abcd-worker-1")).toBeTruthy();
    expect(screen.getByText("Pending")).toBeTruthy();
    expect(screen.getByText("Running")).toBeTruthy();
  });

  it("says when a change takes effect", () => {
    renderIt();
    expect(screen.getByText(/next deploy/i)).toBeTruthy();
    expect(screen.getByText(/immediately/i)).toBeTruthy();
  });

  it("never offers replicas or resource limits", () => {
    renderIt();
    fireEvent.click(screen.getByRole("button", { name: /Add Process Type/ }));
    expect(screen.queryByText(/replicas/i)).toBeNull();
    expect(screen.queryByText(/cpu/i)).toBeNull();
    expect(screen.queryByText(/memory/i)).toBeNull();
  });

  it("does not submit an invalid name, and does not flag it while typing", async () => {
    renderIt();
    fireEvent.click(screen.getByRole("button", { name: /Add Process Type/ }));
    const name = await screen.findByLabelText("Name");

    // Mid-typing a name that is not valid yet: no error before blur.
    fireEvent.change(name, { target: { value: "Bad_Name" } });
    expect(
      screen.queryByText(/starting with a letter/i, {
        selector: "[data-slot]",
      }),
    ).toBeNull();

    const submit = screen
      .getAllByRole("button", { name: "Add Process Type" })
      .at(-1)!;
    fireEvent.click(submit);
    await waitFor(() =>
      expect(screen.getByText(/starting with a letter/i)).toBeTruthy(),
    );
    expect(createMutate).not.toHaveBeenCalled();
  });

  it("creates a type with a valid name", async () => {
    renderIt();
    fireEvent.click(screen.getByRole("button", { name: /Add Process Type/ }));
    fireEvent.change(await screen.findByLabelText("Name"), {
      target: { value: "scheduler" },
    });
    const submit = screen
      .getAllByRole("button", { name: "Add Process Type" })
      .at(-1)!;
    fireEvent.click(submit);
    await waitFor(() => expect(createMutate).toHaveBeenCalledTimes(1));
    expect(createMutate.mock.calls[0][0]).toMatchObject({
      name: "scheduler",
      command: "",
      health_check_type: "",
      health_check_command: "",
    });
  });
});
