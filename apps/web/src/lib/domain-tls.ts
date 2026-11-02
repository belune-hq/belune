// What a domain's TLS settings mean once both are read. ssl_mode decides whether
// there is an HTTPS listener for the name at all; force_https only controls the
// redirect to it. A stored force_https=true on an ssl_mode=off domain (the form
// used to submit it raw while only DISPLAYING it unchecked) therefore means
// nothing, and every surface must say so rather than repeat the stale value.

/** Whether visitors are redirected to HTTPS. Always false when TLS is off. */
export function effectiveForceHTTPS(
  sslMode: string,
  forceHTTPS: boolean,
): boolean {
  return sslMode !== "off" && forceHTTPS;
}

/** The scheme a domain is actually served on. */
export function domainScheme(sslMode: string): "http" | "https" {
  return sslMode === "off" ? "http" : "https";
}

const NOT_RUNNING: Record<string, string> = {
  stopped: "is stopped",
  inactive: "has not been deployed yet",
  error: "is in an error state",
  unhealthy: "is unhealthy",
};

/**
 * Why a domain with TLS on can sit at "pending" with nothing wrong with it, or
 * null when that is not the reason.
 *
 * The proxy routes only a RUNNING application, and a name with no route is never
 * asked for a certificate (a policy for it would attract an issuance nothing
 * serves). So a stopped application's domain stays pending until it runs, and
 * without this sentence nothing on the page says so. ⛔ The gate is deliberate;
 * this is the explanation of it, not a reason to change it.
 *
 * Absent for ssl_mode "off" (no certificate is wanted), for a running
 * application, and for a status this does not know: an unfamiliar status claims
 * nothing rather than guessing.
 */
export function unroutedNotice(
  sslMode: string,
  appStatus: string | undefined,
): string | null {
  if (sslMode === "off" || !appStatus || appStatus === "running") return null;
  const what = NOT_RUNNING[appStatus];
  if (!what) return null;
  return `This application ${what}, so this domain has no route and no certificate is requested. One is requested once the application is running.`;
}
