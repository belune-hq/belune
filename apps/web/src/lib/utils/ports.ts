import type { Application, DomainExpanded } from "@/lib/types";

export const DEFAULT_PORT = 8080;

export interface ResolvedRoute {
  hostname: string;
  path: string;
  port: number;
  /** True when the domain's own port differs from what the app-level port
   *  (or the default) would have given it — i.e. the domain really overrides. */
  overridden: boolean;
}

export interface ResolvedPorts {
  /** The port the deploy health probe uses; null when no HTTP probe runs. */
  healthPort: number | null;
  healthSource: "app" | "domain" | "default";
  routes: ResolvedRoute[];
}

const set = (v: number | null | undefined): v is number =>
  typeof v === "number" && v > 0;

/**
 * What the saved configuration actually resolves to. The two precedences are
 * opposite on purpose and mirror the Go code — keep them in step with it:
 *
 *  - health probe (worker resolveContainerPort): app port, then the first
 *    domain that declares one, then 8080. "First" is the order the API lists
 *    domains, which is newest first.
 *  - proxy route (proxy.ResolveUpstreamPort): the domain's own port, then the
 *    app port, then 8080. Per domain, since each hostname may point elsewhere.
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
  domains: Pick<DomainExpanded, "hostname" | "path" | "container_port">[],
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

  const fallback = appPort ?? DEFAULT_PORT;
  const routes = domains.map((d) => {
    const port = set(d.container_port) ? d.container_port : fallback;
    return {
      hostname: d.hostname,
      path: d.path,
      port,
      overridden: set(d.container_port) && d.container_port !== fallback,
    };
  });

  return { healthPort, healthSource, routes };
}
