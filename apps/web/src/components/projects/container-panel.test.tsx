import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { ServiceInstanceMetrics, ServiceMetrics } from "@/lib/types";

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    params,
  }: {
    children: React.ReactNode;
    to: string;
    params: Record<string, string>;
  }) => (
    <a href={to.replace(/\$(\w+)/g, (_, k: string) => params[k])}>{children}</a>
  ),
}));

import { ContainerPanel, hasContainerPanel } from "./container-panel";

const inst = (
  over: Partial<ServiceInstanceMetrics>,
): ServiceInstanceMetrics => ({
  process_type: "web",
  idx: 1,
  routable: true,
  status: "running",
  cpu_percent: 10,
  memory_used: 1024 * 1024,
  ...over,
});

const metrics = (instances: ServiceInstanceMetrics[]): ServiceMetrics => ({
  cpu_percent: 10,
  memory_used: 1024 * 1024,
  memory_limit: 0,
  uptime_seconds: 0,
  status: "running",
  instances,
  routable_sampled: 0,
  routable_expected: 0,
  sampled: 0,
  expected: 0,
});

describe("hasContainerPanel", () => {
  it("is false for one web container: the row already says it", () => {
    expect(hasContainerPanel(metrics([inst({})]))).toBe(false);
  });
  it("is true for two replicas, or for a worker beside one web", () => {
    expect(hasContainerPanel(metrics([inst({}), inst({ idx: 2 })]))).toBe(true);
    expect(
      hasContainerPanel(
        metrics([inst({}), inst({ process_type: "worker", routable: false })]),
      ),
    ).toBe(true);
  });
  it("is false with no metrics at all, which is what a database row has", () => {
    expect(hasContainerPanel(undefined)).toBe(false);
    expect(hasContainerPanel(metrics([]))).toBe(false);
  });
});

describe("ContainerPanel", () => {
  const worker = inst({
    process_type: "worker",
    idx: 1,
    routable: false,
    cpu_percent: null,
    memory_used: null,
  });

  it("renders a row per container with its status", () => {
    render(
      <ContainerPanel
        projectId="p"
        applicationId="a"
        metrics={metrics([
          inst({ idx: 1, cpu_percent: 12.4 }),
          inst({
            idx: 2,
            status: "exited",
            cpu_percent: null,
            memory_used: null,
          }),
          worker,
        ])}
      />,
    );
    const region = screen.getByRole("region", { name: "Containers" });
    const rows = within(region).getAllByRole("row").slice(1); // drop the header
    expect(rows).toHaveLength(3);
    expect(within(rows[0]).getByText("web-1")).toBeTruthy();
    expect(within(rows[0]).getByText("12%")).toBeTruthy();
    expect(within(rows[1]).getByText("web-2")).toBeTruthy();
    expect(within(rows[2]).getByText("worker-1")).toBeTruthy();
  });

  it("renders unsampled usage as a dash, never 0%", () => {
    render(
      <ContainerPanel
        projectId="p"
        applicationId="a"
        metrics={metrics([inst({}), worker])}
      />,
    );
    const workerRow = screen.getByText("worker-1").closest("tr")!;
    expect(within(workerRow).getAllByText("—")).toHaveLength(2);
    expect(within(workerRow).queryByText("0%")).toBeNull();
    // A sampled idle container is still a real 0%.
  });

  it("still reads 0% for a sampled idle container", () => {
    render(
      <ContainerPanel
        projectId="p"
        applicationId="a"
        metrics={metrics([
          inst({ cpu_percent: 0, memory_used: 0 }),
          inst({ idx: 2 }),
        ])}
      />,
    );
    expect(screen.getByText("0%")).toBeTruthy();
  });

  it("explains the dash and links to that application's Metrics tab", () => {
    render(
      <ContainerPanel
        projectId="p1"
        applicationId="a1"
        metrics={metrics([inst({}), worker])}
      />,
    );
    expect(screen.getByText(/not sampled here/)).toBeTruthy();
    expect(
      screen.getByRole("link", { name: "Open Metrics" }).getAttribute("href"),
    ).toBe("/projects/p1/applications/a1/metrics");
  });

  it("does not mention the dash when every running container was sampled", () => {
    render(
      <ContainerPanel
        projectId="p"
        applicationId="a"
        metrics={metrics([inst({}), inst({ idx: 2 })])}
      />,
    );
    expect(screen.queryByText(/not sampled here/)).toBeNull();
  });
});
