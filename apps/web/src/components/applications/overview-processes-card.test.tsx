import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import type { ProcessType } from "@/lib/types";

let types: ProcessType[] | undefined = [];
vi.mock("@/lib/hooks/use-process-types", () => ({
  useProcessTypes: () => ({ data: types, isLoading: false }),
}));
vi.mock("@tanstack/react-router", () => ({
  Link: ({ children }: { children: React.ReactNode }) => <a>{children}</a>,
}));

import { OverviewProcessesCard } from "./overview-processes-card";

const pt = (over: Partial<ProcessType>): ProcessType =>
  ({
    id: over.name,
    name: "web",
    routable: false,
    replicas: 1,
    instances: [],
    ...over,
  }) as ProcessType;
const run = (n: number) =>
  Array.from({ length: n }, (_, i) => ({
    id: `r${i}`,
    idx: i + 1,
    container_name: "c",
    container_id: null,
    status: "running",
  }));

describe("OverviewProcessesCard", () => {
  it("shows the app as containers across processes, with a status per process", () => {
    types = [
      pt({ name: "web", routable: true, replicas: 2, instances: run(2) }),
      pt({
        name: "queue",
        replicas: 1,
        instances: [
          {
            id: "q",
            idx: 1,
            container_name: "c",
            container_id: null,
            status: "stopped",
          },
        ],
      }),
    ];
    render(<OverviewProcessesCard projectId="p" applicationId="a" />);
    expect(screen.getByText("3 containers across 2 processes")).toBeTruthy();
    expect(screen.getByText("web")).toBeTruthy();
    expect(screen.getByText("×2")).toBeTruthy();
    expect(screen.getByText("receives traffic")).toBeTruthy();
    expect(screen.getByText("2 running")).toBeTruthy();
    // The worker being down is visible on the Overview, not only on the Processes tab.
    expect(screen.getByText("Stopped")).toBeTruthy();
  });

  it("says there are no processes rather than rendering an empty list", () => {
    types = [];
    render(<OverviewProcessesCard projectId="p" applicationId="a" />);
    expect(screen.getByText("No processes.")).toBeTruthy();
  });
});
