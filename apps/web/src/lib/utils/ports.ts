import type { Application, DomainExpanded } from "@/lib/types";

export const DEFAULT_PORT = 8080;

export interface ResolvedPorts {
  /** The port the deploy health probe uses; null when no HTTP probe runs. */
  healthPort: number | null;
  healthSource: "app" | "domain" | "default";
}

const set = (v: number | null | undefined): v is number =>
  typeof v === "number" && v > 0;

/**
 * The port the deploy health probe will use, from the saved configuration.
 * Mirrors the Go worker's resolveContainerPort — keep them in step: app port,
 * then the first domain that declares one, then 8080. "First" is the order the
 * API lists domains, which is newest first. (The proxy route resolves the other
 * way round, domain before app; the domains table shows each route's port, so
 * it is not repeated here.)
 *
 * A zero is "unset" at every step, as in Go. Only an HTTP check with a path
 * probes over a port (a command check runs inside the container; none probes
 * nothing), so healthPort is null otherwise.
 */
export function resolvePorts(
  app: Pick<
    Application,
    "container_port" | "health_check_type" | "health_check_path"
  >,
  domains: Pick<DomainExpanded, "container_port">[],
): ResolvedPorts {
  const appPort = set(app.container_port) ? app.container_port : null;

  let healthPort: number | null = null;
  let healthSource: ResolvedPorts["healthSource"] = "default";
  if (app.health_check_type === "http" && app.health_check_path) {
    const fromDomain = domains.find((d) => set(d.container_port));
    if (appPort !== null) {
      healthPort = appPort;
      healthSource = "app";
    } else if (fromDomain && set(fromDomain.container_port)) {
      healthPort = fromDomain.container_port;
      healthSource = "domain";
    } else {
      healthPort = DEFAULT_PORT;
    }
  }

  return { healthPort, healthSource };
}
