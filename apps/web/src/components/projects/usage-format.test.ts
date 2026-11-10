import { describe, expect, it } from "vitest";

import type { ServiceMetrics } from "@/lib/types";

import { formatCpuCell, formatMemoryCell } from "./usage-format";

const base: ServiceMetrics = {
  cpu_percent: null,
  memory_used: null,
  memory_limit: 0,
  uptime_seconds: 0,
  status: "running",
};

describe("service row usage cells", () => {
  it("renders an unsampled container as a dash, not 0%", () => {
    expect(formatCpuCell(base)).toBe("—");
    expect(formatMemoryCell(base)).toBe("—");
    expect(formatCpuCell(undefined)).toBe("—");
  });

  it("still renders a sampled idle container as 0%", () => {
    expect(formatCpuCell({ ...base, cpu_percent: 0 })).toBe("0%");
  });

  it("renders sampled values", () => {
    const m = { ...base, cpu_percent: 12.5, memory_used: 1024 * 1024 };
    expect(formatCpuCell(m)).toBe("13%");
    expect(formatMemoryCell(m)).toBe("1 MB");
  });
});
