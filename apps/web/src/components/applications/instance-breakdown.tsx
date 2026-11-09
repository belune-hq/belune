import type { AppInstanceMetricPoint, AppMetricPoint } from "@/lib/types";

function formatBytes(bytes: number) {
  const gb = bytes / (1024 * 1024 * 1024);
  if (gb >= 1) return `${gb.toFixed(1)} GB`;
  const mb = bytes / (1024 * 1024);
  if (mb >= 1) return `${mb.toFixed(0)} MB`;
  return `${(bytes / 1024).toFixed(0)} KB`;
}

function fmtCpu(v: number | null | undefined) {
  return v == null ? "No data" : `${v.toFixed(1)}%`;
}

function fmtMem(v: number | null | undefined) {
  return v == null ? "No data" : formatBytes(v);
}

/**
 * One row per live instance from the latest streamed point.
 *
 * The per-instance points exist ONLY on the live stream (history carries totals
 * alone), so there is nothing here until the stream is connected and a point has
 * arrived. A missing value reads "No data", never 0: zero is a claim about a running
 * container, and an unknown is not that.
 */
export function InstanceBreakdown({ point }: { point: AppMetricPoint | null }) {
  const rows = point?.instances;
  if (!rows || rows.length === 0) return null;
  return (
    <div className="mt-6 border-t pt-4" role="region" aria-label="Instances">
      <p className="mb-2 text-sm font-medium">Instances</p>
      <table className="w-full text-sm">
        <thead className="text-muted-foreground text-left text-xs">
          <tr>
            <th className="py-1 pr-3 font-medium">Instance</th>
            <th className="py-1 pr-3 font-medium">CPU</th>
            <th className="py-1 pr-3 font-medium">Memory</th>
            <th className="py-1 font-medium">In the totals</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r: AppInstanceMetricPoint) => (
            <tr key={r.instance_id} className="border-t">
              <td className="py-1.5 pr-3 font-mono text-xs">
                {r.process_type}-{r.idx}
              </td>
              <td className="py-1.5 pr-3">{fmtCpu(r.cpu_percent)}</td>
              <td className="py-1.5 pr-3">{fmtMem(r.memory_usage)}</td>
              <td className="text-muted-foreground py-1.5">
                {r.routable ? "Yes" : "No — worker"}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
