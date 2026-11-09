export type HealthKind = "http" | "command" | "none";

/** Health kinds a type may be given: http needs a port, so only the routable one. */
export function allowedHealthKinds(routable: boolean): HealthKind[] {
  return routable ? ["http", "command", "none"] : ["command", "none"];
}

// Same pattern the server and the database enforce.
export const PROCESS_TYPE_NAME = /^[a-z][a-z0-9-]{0,19}$/;
