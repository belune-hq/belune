import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import type { AppMetricPoint } from "@/lib/types";
import { InstanceBreakdown } from "./instance-breakdown";

const point = (instances: AppMetricPoint["instances"]): AppMetricPoint => ({
  cpu_percent: 30,
  memory_usage: 1,
  memory_limit: 1,
  network_rx_bytes: 0,
  network_tx_bytes: 0,
  recorded_at: "2026-11-09T00:00:00Z",
  instances,
});

const inst = (
  over: Partial<NonNullable<AppMetricPoint["instances"]>[number]>,
) => ({
  instance_id: "i1",
  process_type: "web",
  idx: 1,
  routable: true,
  cpu_percent: 12.5,
  memory_usage: 100 * 1024 * 1024,
  memory_limit: 0,
  network_rx_bytes: 0,
  network_tx_bytes: 0,
  ...over,
});

describe("InstanceBreakdown", () => {
  it("renders nothing without per-instance points (history has none)", () => {
    const { container } = render(
      <InstanceBreakdown point={point(undefined)} />,
    );
    expect(container.firstChild).toBeNull();
    const none = render(<InstanceBreakdown point={null} />);
    expect(none.container.firstChild).toBeNull();
  });

  it("lists every replica and says which are in the totals", () => {
    render(
      <InstanceBreakdown
        point={point([
          inst({ instance_id: "a", idx: 1 }),
          inst({ instance_id: "b", idx: 2, cpu_percent: 99 }),
          inst({ instance_id: "c", process_type: "queue", routable: false }),
        ])}
      />,
    );
    expect(screen.getByText("web-1")).toBeTruthy();
    expect(screen.getByText("web-2")).toBeTruthy();
    expect(screen.getByText("queue-1")).toBeTruthy();
    expect(screen.getByText("99.0%")).toBeTruthy();
    expect(screen.getAllByText("Yes")).toHaveLength(2);
    expect(screen.getByText("No — worker")).toBeTruthy();
  });

  // Zero is a claim about a running container; an unknown value is not that.
  it("renders an unknown value as No data, never as 0", () => {
    render(
      <InstanceBreakdown
        point={point([
          inst({
            cpu_percent: null as unknown as number,
            memory_usage: null as unknown as number,
          }),
        ])}
      />,
    );
    expect(screen.getAllByText("No data")).toHaveLength(2);
    expect(screen.queryByText("0.0%")).toBeNull();
  });
});
