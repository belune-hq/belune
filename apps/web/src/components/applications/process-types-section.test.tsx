import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ProcessType } from "@/lib/types";

const createMutate = vi.fn();
const deleteMutate = vi.fn();
const restartMutate = vi.fn();
const updateMutate = vi.fn();
const toastWarning = vi.fn();
let types: ProcessType[] = [];
let volumes: unknown[] = [];
let appLimits = { cpu_limit: 0, memory_limit: 0 };

vi.mock("@/lib/hooks/use-process-types", () => ({
  useProcessTypes: () => ({ data: types, isLoading: false }),
  useCreateProcessType: () => ({
    mutateAsync: createMutate,
    isPending: false,
  }),
  useUpdateProcessType: () => ({ mutateAsync: updateMutate, isPending: false }),
  useDeleteProcessType: () => ({ mutateAsync: deleteMutate, isPending: false }),
  useRestartInstance: () => ({ mutateAsync: restartMutate, isPending: false }),
}));
vi.mock("@/lib/hooks/use-volumes", () => ({
  useVolumes: () => ({ data: volumes }),
}));
vi.mock("@/lib/hooks/use-applications", () => ({
  useApplication: () => ({ data: appLimits }),
}));
vi.mock("sonner", () => ({
  toast: {
    promise: vi.fn(),
    warning: (m: string) => toastWarning(m),
    error: vi.fn(),
  },
}));

import { ProcessTypesSection } from "./process-types-section";

const pt = (over: Partial<ProcessType>): ProcessType => ({
  id: "id",
  name: "web",
  command: "",
  routable: false,
  replicas: 1,
  cpu_limit: null,
  memory_limit: null,
  health_check_type: null,
  health_check_command: null,
  // What the server resolves for a type with nothing set: the routable one inherits
  // http, a worker gets none. The component renders this; it does not recompute it.
  resolved_health: over.routable
    ? { kind: "http", source: "inherited" }
    : { kind: "none", source: "default" },
  instances: [],
  created_at: "",
  updated_at: "",
  ...over,
});

const renderIt = () =>
  render(<ProcessTypesSection projectId="p" applicationId="a" />);

/**
 * Rendered text and the accessible tree only — jsdom has no layout engine, so
 * whether a person can SEE any of this is the browser drill's job.
 */
describe("ProcessTypesSection", () => {
  beforeEach(() => {
    volumes = [];
    appLimits = { cpu_limit: 0, memory_limit: 0 };
    createMutate.mockReset().mockResolvedValue({});
    deleteMutate.mockReset().mockResolvedValue({ status: "deleted" });
    toastWarning.mockReset();
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

  it("offers replicas and per-type limits when adding a type", async () => {
    renderIt();
    fireEvent.click(screen.getByRole("button", { name: /Add Process Type/ }));
    expect(await screen.findByLabelText("Replicas")).toBeTruthy();
    expect(screen.getByLabelText("CPU (cores)")).toBeTruthy();
    expect(screen.getByLabelText("Memory (MB)")).toBeTruthy();
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

  // The removal can fail after the row is gone, and the server reports that as a
  // warning on a 200. It must reach the operator: a container left running while the
  // row says it is gone is exactly what this exists to surface.
  it("surfaces a removal warning instead of swallowing it", async () => {
    deleteMutate.mockResolvedValue({
      status: "deleted",
      warning: "its container could not be removed yet",
    });
    renderIt();
    fireEvent.click(screen.getByRole("button", { name: "Remove worker" }));
    fireEvent.click(
      await screen.findByRole("button", { name: "Remove process type" }),
    );
    await waitFor(() =>
      expect(toastWarning).toHaveBeenCalledWith(
        "its container could not be removed yet",
      ),
    );
    expect(deleteMutate).toHaveBeenCalledWith("k");
  });

  // The component renders what the SERVER resolved; it does not recompute the rule.
  // A deliberately inconsistent fixture proves it: this "worker" is non-routable with
  // no type of its own, which the old client copy would have shown as "None (default)".
  it("renders the health check from the API, not from a local copy of the rule", () => {
    types = [
      pt({
        id: "k",
        name: "worker",
        resolved_health: { kind: "command", source: "set" },
        health_check_command: "pgrep worker",
      }),
    ];
    renderIt();
    expect(screen.getByText("Command: pgrep worker")).toBeTruthy();
    expect(screen.queryByText("None (default)")).toBeNull();
  });

  it("restarts exactly the instance whose control was used", async () => {
    restartMutate.mockResolvedValue({ status: "restarted" });
    types = [
      pt({
        id: "w",
        name: "web",
        routable: true,
        instances: [
          {
            id: "i-web",
            idx: 1,
            container_name: "app",
            container_id: "c1",
            status: "running",
          },
        ],
      }),
      pt({
        id: "k",
        name: "worker",
        instances: [
          {
            id: "i-worker",
            idx: 1,
            container_name: "app-worker-1",
            container_id: "c2",
            status: "running",
          },
        ],
      }),
    ];
    renderIt();
    fireEvent.click(
      screen.getByRole("button", { name: "Restart app-worker-1" }),
    );
    await waitFor(() => expect(restartMutate).toHaveBeenCalledTimes(1));
    expect(restartMutate).toHaveBeenCalledWith("i-worker");
  });

  it("offers no restart for an instance that has no container yet", () => {
    renderIt();
    // The fixture's worker is 'pending' with container_id null: nothing to restart.
    const btn = screen.getByRole("button", {
      name: "Restart proj-app-1234abcd-worker-1",
    }) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
  });

  // The routable type cannot be paused. The operator is told in their own terms
  // before the round trip; the server refuses the same thing and stays the authority.
  it("refuses 0 replicas on the web process, and offers a pause on a worker", async () => {
    renderIt();
    fireEvent.click(screen.getByRole("button", { name: "Edit web" }));
    const replicas = await screen.findByLabelText("Replicas");
    fireEvent.change(replicas, { target: { value: "0" } });
    fireEvent.blur(replicas);
    await waitFor(() =>
      expect(screen.getByText(/cannot be paused/i)).toBeTruthy(),
    );
  });

  it("labels 0 as a pause on a worker", async () => {
    renderIt();
    fireEvent.click(screen.getByRole("button", { name: "Edit worker" }));
    fireEvent.change(await screen.findByLabelText("Replicas"), {
      target: { value: "0" },
    });
    await waitFor(() => expect(screen.getByText(/Paused\./)).toBeTruthy());
  });

  it("refuses more than one replica of any type when the application has a volume", async () => {
    volumes = [{ id: "v" }];
    renderIt();
    fireEvent.click(screen.getByRole("button", { name: "Edit worker" }));
    const replicas = await screen.findByLabelText("Replicas");
    fireEvent.change(replicas, { target: { value: "2" } });
    fireEvent.blur(replicas);
    await waitFor(() =>
      expect(screen.getByText(/persistent volumes/i)).toBeTruthy(),
    );
  });

  // A blank limit is not "zero": it inherits the application's, and says so.
  it("shows what an empty limit inherits instead of a bare blank", async () => {
    appLimits = { cpu_limit: 2, memory_limit: 512 * 1024 * 1024 };
    renderIt();
    fireEvent.click(screen.getByRole("button", { name: "Edit worker" }));
    expect(
      await screen.findByPlaceholderText("Inherit — 2 cores"),
    ).toBeTruthy();
    expect(screen.getByPlaceholderText("Inherit — 512 MB")).toBeTruthy();
  });

  it("sends replicas and clears blank limits when saving", async () => {
    updateMutate.mockResolvedValue({});
    renderIt();
    fireEvent.click(screen.getByRole("button", { name: "Edit worker" }));
    fireEvent.change(await screen.findByLabelText("Replicas"), {
      target: { value: "3" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(updateMutate).toHaveBeenCalledTimes(1));
    expect(updateMutate.mock.calls[0][0].data).toMatchObject({
      replicas: 3,
      cpu_limit: 0,
      memory_limit: 0,
    });
  });

  it("shows a paused type as paused, with no containers", () => {
    types = [pt({ id: "k", name: "worker", replicas: 0, instances: [] })];
    renderIt();
    expect(screen.getByText("0 — paused")).toBeTruthy();
    expect(screen.getByText("Paused — no containers")).toBeTruthy();
  });
});
