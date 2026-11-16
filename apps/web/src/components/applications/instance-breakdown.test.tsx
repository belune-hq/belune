import { describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { vi } from "vitest";
import type { AppMetricPoint } from "@/lib/types";
import { InstanceBreakdown } from "./instance-breakdown";

// uPlot needs a canvas jsdom does not have. The stub exposes what the chart was handed,
// which is what these specs are about: the series, and whether it bridges gaps.
vi.mock("@/components/ui/uplot-area-chart", () => ({
  UPlotAreaChart: (props: {
    values: (number | null)[];
    spanGaps?: boolean;
  }) => (
    <div
      data-testid="chart"
      data-values={JSON.stringify(props.values)}
      data-spangaps={String(props.spanGaps)}
    />
  ),
}));

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
  memory_limit: null,
  cpu_limit: null,
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

  it("calls them containers, not instances", () => {
    render(<InstanceBreakdown point={point([inst({})])} />);
    expect(screen.getByRole("region", { name: "Containers" })).toBeTruthy();
    expect(
      screen.getByRole("columnheader", { name: "Container" }),
    ).toBeTruthy();
  });

  describe("drill-down", () => {
    const at = (i: number, instances: AppMetricPoint["instances"]) => ({
      ...point(instances),
      recorded_at: `2026-11-09T00:00:0${i}Z`,
    });
    const two = [
      inst({ instance_id: "a", idx: 1 }),
      inst({ instance_id: "b", idx: 2 }),
    ];

    it("expands one row at a time, with aria-expanded and a controlled panel", () => {
      render(<InstanceBreakdown point={at(0, two)} window={[at(0, two)]} />);
      const a = screen.getByRole("button", { name: /web-1/ });
      const b = screen.getByRole("button", { name: /web-2/ });
      expect(a.getAttribute("aria-expanded")).toBe("false");
      expect(screen.queryAllByTestId("chart")).toHaveLength(0);

      fireEvent.click(a);
      expect(a.getAttribute("aria-expanded")).toBe("true");
      expect(
        document.getElementById(a.getAttribute("aria-controls")!),
      ).toBeTruthy();
      expect(screen.getAllByTestId("chart")).toHaveLength(2); // CPU + memory

      fireEvent.click(b);
      expect(a.getAttribute("aria-expanded")).toBe("false");
      expect(b.getAttribute("aria-expanded")).toBe("true");
      expect(screen.getAllByTestId("chart")).toHaveLength(2); // still one panel

      fireEvent.click(b);
      expect(screen.queryAllByTestId("chart")).toHaveLength(0);
    });

    it("draws a missed sample as a null gap, and does not bridge it", () => {
      const window = [
        at(0, [inst({ instance_id: "a", cpu_percent: 5 })]),
        at(1, [inst({ instance_id: "b", cpu_percent: 9 })]), // a absent
        at(2, [inst({ instance_id: "a", cpu_percent: 7 })]),
      ];
      render(<InstanceBreakdown point={window[2]} window={window} />);
      fireEvent.click(screen.getByRole("button", { name: /web-1/ }));
      const cpu = screen.getAllByTestId("chart")[0];
      expect(cpu.getAttribute("data-values")).toBe("[5,null,7]");
      expect(cpu.getAttribute("data-spangaps")).toBe("false");
    });

    it("shows the configured limits, and unlimited for null rather than 0 or host RAM", () => {
      const limited = inst({
        instance_id: "a",
        idx: 1,
        cpu_limit: 0.5,
        memory_limit: 512 * 1024 * 1024,
      });
      const unlimited = inst({ instance_id: "b", idx: 2 });
      render(
        <InstanceBreakdown
          point={at(0, [limited, unlimited])}
          window={[at(0, [limited, unlimited])]}
        />,
      );
      fireEvent.click(screen.getByRole("button", { name: /web-1/ }));
      expect(screen.getByText(/Limit: CPU 0\.5 · Memory 512 MB/)).toBeTruthy();
      fireEvent.click(screen.getByRole("button", { name: /web-2/ }));
      expect(
        screen.getByText(/Limit: CPU unlimited · Memory unlimited/),
      ).toBeTruthy();
    });

    it("keeps the panel and its history when the open container leaves the latest sample", () => {
      const first = at(0, two);
      const { rerender } = render(
        <InstanceBreakdown point={first} window={[first]} />,
      );
      fireEvent.click(screen.getByRole("button", { name: /web-2/ }));

      const later = at(1, [inst({ instance_id: "a", idx: 1 })]); // b gone
      rerender(<InstanceBreakdown point={later} window={[first, later]} />);
      expect(screen.getByText(/not in the latest sample/)).toBeTruthy();
      const cpu = screen.getAllByTestId("chart")[0];
      expect(cpu.getAttribute("data-values")).toBe("[12.5,null]");
    });
  });
});
