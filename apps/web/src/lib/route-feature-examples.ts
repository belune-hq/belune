import type { RouteFeature } from "@/lib/types";

type FeatureType = RouteFeature["feature_type"];

// Prefilled when a type is picked, so nobody has to guess the shape. The server
// parses with DisallowUnknownFields (ParseFeatureConfig in
// apps/api/internal/proxy/features.go), so one wrong key makes an example that
// is rejected with an "unknown field" message: every key here is read from the
// Go structs, and route-feature-examples.test.ts re-derives them from that file
// so a typo or a renamed field cannot ship.
//
// Every example is a working config, except basic_auth's, on purpose: an example
// must never carry a credential. A pre-filled "admin" and "changeme" would become
// a real gate with a known password the moment someone clicked Add, so basic_auth
// shows the shape and leaves both values empty, and the server refuses it until
// they are filled in ("username and password are required").
//
// It sends `password`, which the server hashes before storing (see
// NormalizeFeatureConfig in apps/api/internal/proxy). `hashed_password` is the
// other accepted key, for a bcrypt hash someone already has; the example omits it
// because the server needs only one of the two.
//
// Exhaustive over the type union, so a new feature type without an example is a
// compile error and not a field that silently falls back to "{}".
export const CONFIG_EXAMPLES: Record<FeatureType, string> = {
  basic_auth: '{"username":"","password":""}',
  headers: '{"response":{"set":{"X-Frame-Options":["DENY"]}}}',
  // 203.0.113.0/24 is the RFC 5737 documentation range, so nobody mistakes it
  // for a network they should keep.
  ip_allowlist: '{"ranges":["203.0.113.0/24"]}',
  redirect: '{"from":"/old","to":"/new","status_code":301}',
  rate_limit: '{"window":"1s","max_events":10}',
};

/** The example for a feature type, or undefined for an unknown or unset one. */
export function exampleFor(type: string): string | undefined {
  return Object.hasOwn(CONFIG_EXAMPLES, type)
    ? CONFIG_EXAMPLES[type as FeatureType]
    : undefined;
}

const EXAMPLES = new Set(Object.values(CONFIG_EXAMPLES));

/**
 * What the config field should read after the feature type changes to `type`.
 *
 * Replaces the value only if the person has not touched it: empty, the bare
 * "{}" it starts as, or exactly some type's example. Anything else was typed,
 * and silently discarding typed work is far worse than leaving a stale example.
 *
 * "Exactly any type's example", not just "{}": once every type has an example,
 * picking Rate Limit and then changing your mind leaves the field holding a
 * rate_limit config, which is not "{}", so a rule that only recognised "{}"
 * would leave it there under a Basic Auth label.
 */
export function configForType(current: string, type: string): string {
  const untouched = current === "" || current === "{}" || EXAMPLES.has(current);
  return untouched ? (exampleFor(type) ?? "{}") : current;
}
