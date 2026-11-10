import type { ServiceMetrics } from "@/lib/types";

function formatBytes(bytes: number): string {
  const gb = bytes / (1024 * 1024 * 1024);
  if (gb >= 1) return `${gb.toFixed(1)} GB`;
  const mb = bytes / (1024 * 1024);
  if (mb >= 1) return `${mb.toFixed(0)} MB`;
  return `${(bytes / 1024).toFixed(0)} KB`;
}

/** CPU cell: a dash for an unknown, never a confident 0%. */
export function formatCpuCell(m: ServiceMetrics | null | undefined): string {
  return m && m.cpu_percent != null ? `${m.cpu_percent.toFixed(0)}%` : "—";
}

export function formatMemoryCell(m: ServiceMetrics | null | undefined): string {
  return m && m.memory_used ? formatBytes(m.memory_used) : "—";
}
