import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { Application, DomainExpanded } from "@/lib/types";

const mutateAsync = vi.fn();
let domains: Partial<DomainExpanded>[] = [];

vi.mock("@/lib/hooks/use-applications", () => ({
  useSetContainerPort: () => ({ mutateAsync, isPending: false }),
}));
vi.mock("@/lib/hooks/use-domains", () => ({
  useDomains: () => ({ data: domains }),
}));
vi.mock("sonner", () => ({
  toast: { promise: vi.fn(), error: vi.fn() },
}));

import { PortSection } from "./port-section";

const app = (over: Partial<Application> = {}) =>
  ({
    container_port: null,
    health_check_type: "http",
    health_check_path: "/up",
    ...over,
  }) as Application;

// Collapse whitespace so assertions read the way a person reads the block.
const textOf = (el: HTMLElement) => el.textContent.replace(/\s+/g, " ").trim();

const renderIt = (a: Application) =>
  render(<PortSection projectId="p" applicationId="a" application={a} />);

// Asserts text content only. jsdom has no layout engine, so this cannot say the
// resolution block is visible to a person — that is the browser drill's job.
describe("PortSection", () => {
  beforeEach(() => {
    mutateAsync.mockReset().mockResolvedValue({});
    domains = [];
  });

  it("shows the probe pinned to the app port and each route's own port", () => {
    domains = [
      { hostname: "admin.example.com", path: "/", container_port: 3001 },
      { hostname: "user.example.com", path: "/", container_port: 3000 },
    ];
    renderIt(app({ container_port: 3000 }));

    const block = screen.getByLabelText("Resolved ports");
    expect(textOf(block)).toContain("Health checks probe :3000");
    expect(textOf(block)).toContain(
      "admin.example.com → :3001 (set on the domain)",
    );
    expect(textOf(block)).toContain("user.example.com → :3000");
    expect(textOf(block)).not.toContain("user.example.com → :3000 (set");
  });

  it("says plainly when no HTTP check probes a port", () => {
    renderIt(app({ health_check_type: "none", health_check_path: null }));
    expect(textOf(screen.getByLabelText("Resolved ports"))).toContain(
      "no HTTP health check is configured",
    );
  });

  it("sends the number typed, and 0 when the field is cleared", async () => {
    renderIt(app({ container_port: 3000 }));
    const input = screen.getByLabelText("Container Port");
    const save = () =>
      fireEvent.click(screen.getByRole("button", { name: "Save" }));

    fireEvent.change(input, { target: { value: "4000" } });
    save();
    await waitFor(() =>
      expect(mutateAsync).toHaveBeenLastCalledWith({ container_port: 4000 }),
    );

    fireEvent.change(input, { target: { value: "" } });
    save();
    await waitFor(() =>
      expect(mutateAsync).toHaveBeenLastCalledWith({ container_port: 0 }),
    );
  });

  it("refuses an out-of-range port without calling the API", async () => {
    renderIt(app());
    fireEvent.change(screen.getByLabelText("Container Port"), {
      target: { value: "99999" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(
      await screen.findByText("Port must be between 1 and 65535"),
    ).toBeTruthy();
    expect(mutateAsync).not.toHaveBeenCalled();
  });
});
