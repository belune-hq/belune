import { Link } from "@tanstack/react-router";
import { StatusPill } from "@/components/ui/status-pill";
import type { ServiceMetrics } from "@/lib/types";
import { formatCpuCell, formatMemoryCell } from "./usage-format";

/**
 * Each container of one application: its status, CPU and memory.
 *
 * ⛔ Reads the snapshot the table already holds and opens no query of its own: the
 * project metrics poll is the most expensive request in the app (every running replica is
 * a ~2s Docker stats call), and a panel that fetched would multiply it per open row.
 *
 * Status is Docker's and always present. CPU and memory are only the containers the
 * overview sampled, so a worker (or a replica that did not answer) reads "—", never 0%:
 * "running, usage not sampled here", with the Metrics tab for its actual numbers.
 */
export function ContainerPanel({
  projectId,
  applicationId,
  metrics,
}: {
  projectId: string;
  applicationId: string;
  metrics: ServiceMetrics;
}) {
  const list = metrics.instances;
  const unsampled = list.some(
    (i) => i.status === "running" && i.cpu_percent == null,
  );
  return (
    <div role="region" aria-label="Containers" className="space-y-2 py-1">
      <table className="w-full text-sm">
        <thead className="text-muted-foreground text-left text-xs">
          <tr>
            <th className="py-1 pr-3 font-medium">Container</th>
            <th className="py-1 pr-3 font-medium">Status</th>
            <th className="py-1 pr-3 font-medium">CPU</th>
            <th className="py-1 font-medium">Memory</th>
          </tr>
        </thead>
        <tbody>
          {list.map((i) => (
            <tr key={`${i.process_type}-${i.idx}`} className="border-t">
              <td className="py-1.5 pr-3 font-mono text-xs">
                {i.process_type}-{i.idx}
              </td>
              <td className="py-1.5 pr-3">
                <StatusPill status={i.status} />
              </td>
              <td className="text-text-muted py-1.5 pr-3 font-mono text-xs">
                {formatCpuCell(i)}
              </td>
              <td className="text-text-muted py-1.5 font-mono text-xs">
                {formatMemoryCell(i)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="text-text-faint text-xs">
        {unsampled ? "— means usage is not sampled here. " : ""}
        <Link
          to="/projects/$projectId/applications/$applicationId/metrics"
          params={{ projectId, applicationId }}
          className="text-brand-text hover:underline"
        >
          Open Metrics
        </Link>{" "}
        for each container&apos;s live numbers.
      </p>
    </div>
  );
}
