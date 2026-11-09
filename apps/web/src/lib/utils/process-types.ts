export type HealthKind = "http" | "command" | "none";

/** Health kinds a type may be given: http needs a port, so only the routable one. */
export function allowedHealthKinds(routable: boolean): HealthKind[] {
  return routable ? ["http", "command", "none"] : ["command", "none"];
}

// Same pattern the server and the database enforce.
export const PROCESS_TYPE_NAME = /^[a-z][a-z0-9-]{0,19}$/;

export const MAX_REPLICAS = 20;

/**
 * The client side of the replica rules. The server stays the authority and refuses the
 * same things; this exists so the operator is told before the round trip, in their own
 * terms.
 *
 *  - 0 is "paused" and is legal on a worker only: the routable type has the domains
 *    routed to it, so with no container the proxy would have nothing to send to.
 *  - more than one is refused for EVERY type of an application with a volume: volumes
 *    belong to the application and mount into every container, so two containers would
 *    write the same one.
 */
export function validateReplicas(
  raw: string,
  opts: { routable: boolean; hasVolumes: boolean },
): string | undefined {
  const text = raw.trim();
  if (text === "") return "Enter a number of replicas";
  const n = Number(text);
  if (!Number.isInteger(n)) return "Replicas must be a whole number";
  if (n < 0 || n > MAX_REPLICAS)
    return `Replicas must be between 0 and ${MAX_REPLICAS}`;
  if (n === 0 && opts.routable)
    return "The web process cannot be paused: domains route to it, so it needs at least 1 replica";
  if (n > 1 && opts.hasVolumes)
    return "This application has persistent volumes, and every container mounts them, so more than one replica would write the same volume. Remove the volumes or keep 1 replica";
  return undefined;
}

/** Blank means inherit the application's; anything else must be a positive number. */
export function validateLimit(raw: string, label: string): string | undefined {
  const text = raw.trim();
  if (text === "") return undefined;
  const n = Number(text);
  if (!Number.isFinite(n) || n <= 0)
    return `${label} must be a positive number`;
  return undefined;
}

export const MIN_MEMORY_MB = 6;

export function validateMemoryMb(raw: string): string | undefined {
  const err = validateLimit(raw, "Memory");
  if (err || raw.trim() === "") return err;
  if (Number(raw) < MIN_MEMORY_MB)
    return `Memory must be at least ${MIN_MEMORY_MB} MB`;
  return undefined;
}
