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
