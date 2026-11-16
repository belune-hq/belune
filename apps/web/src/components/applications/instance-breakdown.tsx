import { useId, useState } from "react";
import { ChevronRightIcon } from "lucide-react";
import { UPlotAreaChart } from "@/components/ui/uplot-area-chart";
import { instanceSeries } from "@/lib/utils/process-usage";
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

function fmtTime(ts: number) {
  return new Date(ts).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/**
 * "CPU 0.5 · Memory 512 MB". Each part is named and an unknown one says "unlimited":
 * null is unlimited, never 0 and never host RAM, and dropping it would hide the half
 * of the ceiling that is missing.
 */
function fmtLimits(
  r: Pick<AppInstanceMetricPoint, "cpu_limit" | "memory_limit">,
) {
  const cpu = r.cpu_limit != null ? String(r.cpu_limit) : "unlimited";
  const mem =
    r.memory_limit != null ? formatBytes(r.memory_limit) : "unlimited";
  return `CPU ${cpu} · Memory ${mem}`;
}

const COLS = 4;

/**
 * One row per live instance from the latest streamed point; a row expands into that
 * container's own CPU and memory over the stream window.
 *
 * The per-instance points exist ONLY on the live stream (history carries totals
 * alone), so there is nothing here until the stream is connected and a point has
 * arrived. A missing value reads "No data", never 0: zero is a claim about a running
 * container, and an unknown is not that.
 *
 * A table, not a dropdown: the question is "which one is hot", and a table answers it at
 * a glance where a dropdown makes you click through N containers. One row open at a time:
 * compare in the table, then inspect one.
 */
export function InstanceBreakdown({
  point,
  window: points = [],
}: {
  point: AppMetricPoint | null;
  /** The stream window the series are drawn from; omit for the table alone. */
  window?: readonly AppMetricPoint[];
}) {
  // Keyed on instance_id, the instance ROW's id. A rolling deploy repoints the row at
  // cutover (the row always describes the CURRENT container), so the id survives a roll:
  // the panel stays open and the series splices the old and new container into one line.
  // That is the right identity (it is what the operator means by "replica 0"), but a step
  // change in memory at cutover is the new container, not a fault. During a roll only one
  // generation is sampled at a time, so a container that stops appearing is not
  // necessarily gone.
  const [openId, setOpenId] = useState<string | null>(null);
  const baseId = useId();
  const list = point?.instances ?? [];
  if (list.length === 0 && openId == null) return null;
  // The open instance may have left the latest point (scaled down, stopped, not
  // answering). The panel stays, and says so; its earlier samples are real data.
  const absentOpen =
    openId != null && !list.some((r) => r.instance_id === openId);

  return (
    <div className="mt-6 border-t pt-4" role="region" aria-label="Containers">
      <p className="mb-2 text-sm font-medium">Containers</p>
      <table className="w-full text-sm">
        <thead className="text-muted-foreground text-left text-xs">
          <tr>
            <th className="py-1 pr-3 font-medium">Container</th>
            <th className="py-1 pr-3 font-medium">CPU</th>
            <th className="py-1 pr-3 font-medium">Memory</th>
            <th className="py-1 font-medium">In the totals</th>
          </tr>
        </thead>
        <tbody>
          {list.map((r: AppInstanceMetricPoint) => {
            const open = r.instance_id === openId;
            return (
              <InstanceRows
                key={r.instance_id}
                r={r}
                open={open}
                panelId={`${baseId}-${r.instance_id}`}
                points={points}
                onToggle={() => setOpenId(open ? null : r.instance_id)}
              />
            );
          })}
          {absentOpen && (
            <tr className="border-t">
              <td colSpan={COLS} className="py-3">
                <p className="text-muted-foreground mb-2 text-xs">
                  This container is not in the latest sample (stopped, scaled
                  down, or not answering). Its earlier samples are below.
                </p>
                <InstanceDetail r={null} id={openId} points={points} />
                <button
                  type="button"
                  className="text-brand-text mt-2 text-xs"
                  onClick={() => setOpenId(null)}
                >
                  Close
                </button>
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  );
}

function InstanceRows({
  r,
  open,
  panelId,
  points,
  onToggle,
}: {
  r: AppInstanceMetricPoint;
  open: boolean;
  panelId: string;
  points: readonly AppMetricPoint[];
  onToggle: () => void;
}) {
  const name = `${r.process_type}-${r.idx}`;
  return (
    <>
      <tr className="border-t">
        <td className="py-1.5 pr-3 font-mono text-xs">
          <button
            type="button"
            aria-expanded={open}
            aria-controls={panelId}
            onClick={onToggle}
            className="flex items-center gap-1"
          >
            <ChevronRightIcon
              aria-hidden="true"
              className={`size-3.5 transition-transform ${open ? "rotate-90" : ""}`}
            />
            {name}
          </button>
        </td>
        <td className="py-1.5 pr-3">{fmtCpu(r.cpu_percent)}</td>
        <td className="py-1.5 pr-3">{fmtMem(r.memory_usage)}</td>
        <td className="text-muted-foreground py-1.5">
          {r.routable ? "Yes" : "No — worker"}
        </td>
      </tr>
      {open && (
        <tr id={panelId}>
          <td colSpan={COLS} className="pb-3">
            <InstanceDetail r={r} id={r.instance_id} points={points} />
          </td>
        </tr>
      )}
    </>
  );
}

function InstanceDetail({
  r,
  id,
  points,
}: {
  r: AppInstanceMetricPoint | null;
  id: string;
  points: readonly AppMetricPoint[];
}) {
  const timestamps = points.map((p) => new Date(p.recorded_at).getTime());
  // spanGaps off: a tick where this container is absent is a gap in the line, not
  // something to interpolate across.
  const chart = (
    title: string,
    values: (number | null)[],
    color: string,
    formatter: (v: number) => string,
  ) => (
    <div role="region" aria-label={`${title} history`}>
      <p className="mb-1 text-xs font-medium">{title}</p>
      <UPlotAreaChart
        timestamps={timestamps}
        values={values}
        color={color}
        yFormatter={formatter}
        xFormatter={fmtTime}
        height={120}
        spanGaps={false}
      />
    </div>
  );
  return (
    <div className="space-y-3">
      {r && (
        <p className="text-muted-foreground text-xs">
          Limit: {fmtLimits(r)} · RX {formatBytes(r.network_rx_bytes)} · TX{" "}
          {formatBytes(r.network_tx_bytes)}
        </p>
      )}
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        {chart(
          "CPU",
          instanceSeries(points, id, (i) => i.cpu_percent),
          "hsl(221, 83%, 53%)",
          (v) => `${v.toFixed(1)}%`,
        )}
        {chart(
          "Memory",
          instanceSeries(points, id, (i) => i.memory_usage),
          "hsl(262, 83%, 58%)",
          formatBytes,
        )}
      </div>
    </div>
  );
}
