import type { ServiceMetrics } from "@/lib/types";

// Structural, so the application row and one of its containers share the formatting: both
// carry the same two nullable usage fields.
type CpuUsage = { cpu_percent: number | null };
type MemoryUsage = { memory_used: number | null };

function formatBytes(bytes: number): string {
  const gb = bytes / (1024 * 1024 * 1024);
  if (gb >= 1) return `${gb.toFixed(1)} GB`;
  const mb = bytes / (1024 * 1024);
  if (mb >= 1) return `${mb.toFixed(0)} MB`;
  return `${(bytes / 1024).toFixed(0)} KB`;
}

/** CPU cell: a dash for an unknown, never a confident 0%. */
export function formatCpuCell(m: CpuUsage | null | undefined): string {
  return m && m.cpu_percent != null ? `${m.cpu_percent.toFixed(0)}%` : "—";
}

export function formatMemoryCell(m: MemoryUsage | null | undefined): string {
  return m && m.memory_used != null ? formatBytes(m.memory_used) : "—";
}

/**
 * Whether an application row has anything to open onto: more than one container, or any
 * that is not of the routable type. One web container is exactly what the row already
 * says, so it gets no chevron (and a database, which has no metrics here, never does).
 */
export function hasContainerPanel(m: ServiceMetrics | undefined): boolean {
  const list = m?.instances ?? [];
  return list.length > 1 || list.some((i) => !i.routable);
}
