import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { Application, DomainExpanded } from "@/lib/types";

const mutateAsync = vi.fn();
let domains: Partial<DomainExpanded>[] = [];

vi.mock("@/lib/hooks/use-applications", () => ({
  useSetHealthCheck: () => ({ mutateAsync, isPending: false }),
}));
vi.mock("@/lib/hooks/use-domains", () => ({
  useDomains: () => ({ data: domains }),
}));
vi.mock("sonner", () => ({
  toast: { promise: vi.fn(), error: vi.fn() },
}));

import { HealthCheckSection } from "./health-check-section";

const app = (over: Partial<Application> = {}) =>
  ({
    container_port: null,
    health_check_type: "http",
    health_check_path: "",
    health_check_command: "",
    ...over,
  }) as Application;

const renderIt = (a: Application = app()) =>
  render(
    <HealthCheckSection projectId="p" applicationId="a" application={a} />,
  );

const method = (name: string) =>
  fireEvent.click(screen.getByRole("button", { name }));

/**
 * Asserts rendered text and the invalid flag only — jsdom has no layout engine,
 * so whether a person can SEE the port field is the browser drill's job
 * (e2e/health-check-port.spec.ts). What is covered here is which messages exist.
 */
describe("HealthCheckSection", () => {
  beforeEach(() => {
    mutateAsync.mockReset().mockResolvedValue({});
    domains = [];
  });

  // The bug: onChangeListenTo on "type" ran the validator as the field
  // unmounted, so the parked error rendered when the method came back.
  it("never flags an untouched required field while the method changes", () => {
    renderIt();
    for (const m of ["Command", "HTTP", "None", "HTTP", "Command"]) {
      method(m);
      expect(screen.queryByText("Path is required")).toBeNull();
      expect(screen.queryByText("Command is required")).toBeNull();
    }
  });

  it("still flags a required field once it has been visited and left empty", () => {
    renderIt();
    const path = screen.getByPlaceholderText("/healthz");
    fireEvent.change(path, { target: { value: "/x" } });
    fireEvent.change(path, { target: { value: "" } });
    expect(screen.getByText("Path is required")).toBeTruthy();
    expect(path.getAttribute("aria-invalid")).toBe("true");
  });

  it("keeps Save disabled while a required field is empty", () => {
    renderIt();
    expect(
      screen.getByRole("button", { name: "Save" }).hasAttribute("disabled"),
    ).toBe(true);
    fireEvent.change(screen.getByPlaceholderText("/healthz"), {
      target: { value: "/up" },
    });
    expect(
      screen.getByRole("button", { name: "Save" }).hasAttribute("disabled"),
    ).toBe(false);
  });

  // A blank port clears it, and the endpoint spells a clear as 0 (absent means
  // "leave the stored port alone"), so blank must not be sent as undefined.
  it("sends a blank container port as 0 and a set one as a number", async () => {
    renderIt(app({ health_check_path: "/up", container_port: 3000 }));
    const port = screen.getByLabelText("Container Port");

    fireEvent.change(port, { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(mutateAsync).toHaveBeenLastCalledWith(
        expect.objectContaining({ container_port: 0 }),
      ),
    );

    fireEvent.change(port, { target: { value: "4000" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(mutateAsync).toHaveBeenLastCalledWith(
        expect.objectContaining({ container_port: 4000 }),
      ),
    );
  });
});
