import type { StatusTone } from "@/components/ui/status-pill";
import type {
  AppInstanceMetricPoint,
  AppMetricPoint,
  ProcessType,
} from "@/lib/types";

/**
 * What the application costs across EVERY container, summed on the client from the
 * point's per-instance samples.
 *
 * The point's own totals (cpu_percent and friends) are the ROUTABLE type's sum and always
 * have been: that is what the Overview showed before there were process types. This is
 * deliberately a different number (workers included) and the Overview says so, because an
 * app with a worker would otherwise just seem to report more CPU than it used to.
 *
 * Each value is null when the point carries no instance samples: an unknown, never 0.
 */
export interface AllProcessUsage {
  cpu: number | null;
  memory: number | null;
  rx: number | null;
  tx: number | null;
}

export function sumAllContainers(
  point: AppMetricPoint | null | undefined,
): AllProcessUsage {
  const rows = point?.instances;
  if (!rows || rows.length === 0) {
    return { cpu: null, memory: null, rx: null, tx: null };
  }
  const sum = (pick: (r: AppInstanceMetricPoint) => number) =>
    rows.reduce((acc, r) => acc + pick(r), 0);
  return {
    cpu: sum((r) => r.cpu_percent),
    memory: sum((r) => r.memory_usage),
    rx: sum((r) => r.network_rx_bytes),
    tx: sum((r) => r.network_tx_bytes),
  };
}

export interface Coverage {
  /** True when fewer containers answered than were expected. */
  partial: boolean;
  text: string;
}

/**
 * Says what the sum covers, or null when there is nothing worth saying (one container,
 * all sampled: the figure means what it always did).
 *
 * ⚠️ "expected" is the set the SERVER sampled against, which filters on each instance
 * ROW's status. A container that is genuinely up while its row still says "pending" is in
 * neither count: invisible, not under-reported. "4 of 4" means full coverage of the
 * intended set, not of reality.
 */
export function describeCoverage(
  point: AppMetricPoint | null | undefined,
): Coverage | null {
  const sampled = point?.sampled;
  const expected = point?.expected;
  if (sampled == null || expected == null || expected === 0) return null;
  if (sampled < expected) {
    const missing = expected - sampled;
    return {
      partial: true,
      text: `Sum of ${sampled} of ${expected} containers (${missing} not sampled)`,
    };
  }
  if (expected === 1) return null;
  return { partial: false, text: `All processes · ${expected} containers` };
}

/**
 * The memory ceiling to show beside the memory figure, or null when it would be a lie.
 *
 * The ceiling is CONFIGURATION for the ROUTABLE type's expected replicas; usage is a
 * measurement over whatever was sampled. They only describe the same containers when
 * every sampled container is routable (no worker in the sum) and every expected routable
 * replica was sampled. Otherwise "12 MB of 512 MB" would put a sum that includes workers
 * over a ceiling that does not, or two replicas' usage over three replicas' ceiling.
 * Null also means "a routable replica is unlimited": there is no ceiling to show.
 */
export function comparableCeiling(
  point: AppMetricPoint | null | undefined,
): number | null {
  const limit = point?.memory_limit;
  const rows = point?.instances;
  if (!limit || !rows || rows.length === 0) return null;
  if (!rows.every((r) => r.routable)) return null;
  if (point.routable_sampled !== point.routable_expected) return null;
  return limit;
}

export interface ProcessStatus {
  label: string;
  tone: StatusTone;
}

/**
 * One status for a process type, from its instances: all running, some running, or the
 * state the instances are in. A header pill is ONE value for N containers; this is what
 * says that the worker is down while web is up.
 */
export function summariseProcess(pt: ProcessType): ProcessStatus {
  if (pt.replicas === 0) return { label: "Paused", tone: "neutral" };
  const total = pt.instances.length;
  if (total === 0) return { label: "Not deployed", tone: "neutral" };
  const running = pt.instances.filter((i) => i.status === "running").length;
  if (running === total) {
    return {
      label: total === 1 ? "Running" : `${running} running`,
      tone: "ready",
    };
  }
  if (running > 0) {
    return { label: `${running} of ${total} running`, tone: "building" };
  }
  const state = pt.instances[0].status;
  const label = state.charAt(0).toUpperCase() + state.slice(1);
  if (state === "error" || state === "unhealthy") {
    return { label, tone: "error" };
  }
  return { label, tone: state === "pending" ? "building" : "neutral" };
}
