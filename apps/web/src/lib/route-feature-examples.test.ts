import { describe, expect, it } from "vitest";
// ?raw, not node:fs: tsconfig.app.json carries only vite/client types, so Node's
// modules are (rightly) not visible to code under src/.
import GO_SOURCE from "../../../api/internal/proxy/features.go?raw";
import {
  CONFIG_EXAMPLES,
  configForType,
  exampleFor,
} from "@/lib/route-feature-examples";

// The server is the authority on what a config looks like, and it is strict:
// ParseFeatureConfig decodes with DisallowUnknownFields, so a single wrong key
// makes an example the API rejects with a confusing "unknown field" message. A
// check that only JSON.parses would pass `{"usrname": ""}`. So this reads the Go
// file itself — the feature types, which struct each one decodes into, and that
// struct's `json:"…"` tags — and holds the examples to them. Rename a field in
// Go and this fails here, rather than in front of an operator.

// const FeatureBasicAuth = "basic_auth"
const CONSTANTS = new Map(
  [...GO_SOURCE.matchAll(/\b(Feature\w+)\s*=\s*"([a-z_]+)"/g)].map((m) => [
    m[1],
    m[2],
  ]),
);

// case FeatureBasicAuth:  …  var c BasicAuthConfig
const STRUCT_OF_TYPE = new Map(
  [...GO_SOURCE.matchAll(/case (Feature\w+):\s*\n\s*var c (\w+)/g)].map((m) => [
    CONSTANTS.get(m[1]),
    m[2],
  ]),
);

// type X struct { Field Type `json:"key,omitempty"` … }  →  X → { key → Go type }
const STRUCTS = new Map<string, Map<string, string>>(
  [...GO_SOURCE.matchAll(/^type (\w+) struct \{\n([\s\S]*?)^\}/gm)].map((m) => [
    m[1],
    new Map(
      [...m[2].matchAll(/^\s*\w+\s+(\S+)\s+`json:"([^",]+)[^"]*"`/gm)].map(
        (f) => [f[2], f[1]],
      ),
    ),
  ]),
);

const TYPES = [...STRUCT_OF_TYPE.keys()] as string[];

function fieldsOf(structName: string): Map<string, string> {
  const fields = STRUCTS.get(structName);
  if (!fields) throw new Error(`no struct ${structName} in features.go`);
  return fields;
}

/** Keys the server's DisallowUnknownFields would refuse, as dotted paths. */
function unknownKeys(value: unknown, structName: string, path = ""): string[] {
  if (typeof value !== "object" || value === null || Array.isArray(value))
    return [];
  const fields = fieldsOf(structName);
  return Object.entries(value).flatMap(([key, child]) => {
    const goType = fields.get(key);
    if (goType === undefined) return [`${path}${key}`];
    const nested = goType.replace(/^\*/, "");
    return STRUCTS.has(nested)
      ? unknownKeys(child, nested, `${path}${key}.`)
      : [];
  });
}

describe("the Go source these tests read", () => {
  // Without this, a regex that matched nothing would make every check below
  // pass on an empty set.
  it("is found and understood", () => {
    expect(TYPES.length).toBeGreaterThanOrEqual(5);
    for (const type of TYPES) {
      expect(fieldsOf(STRUCT_OF_TYPE.get(type)!).size).toBeGreaterThan(0);
    }
    expect(fieldsOf("RateLimitConfig").get("max_events")).toBe("int");
    // A nested struct is followed through the pointer.
    expect(fieldsOf("HeadersConfig").get("response")).toBe("*HeaderOps");
    expect([...fieldsOf("HeaderOps").keys()].sort()).toEqual([
      "add",
      "delete",
      "set",
    ]);
  });
});

describe("CONFIG_EXAMPLES", () => {
  it("has an example for every feature type the server knows, and no others", () => {
    expect(Object.keys(CONFIG_EXAMPLES).sort()).toEqual([...TYPES].sort());
  });

  it.each(TYPES)("%s: is a JSON object", (type) => {
    const parsed: unknown = JSON.parse(CONFIG_EXAMPLES[type as never]);
    expect(typeof parsed).toBe("object");
    expect(parsed).not.toBeNull();
    expect(Array.isArray(parsed)).toBe(false);
  });

  it.each(TYPES)("%s: uses only keys its Go struct declares", (type) => {
    const parsed: unknown = JSON.parse(CONFIG_EXAMPLES[type as never]);
    expect(unknownKeys(parsed, STRUCT_OF_TYPE.get(type)!)).toEqual([]);
  });

  // The other half of "exact key set": a key missing from an example is a field
  // the operator never learns exists, and a field added to the Go struct should
  // make someone decide whether the example shows it. headers is the one
  // deliberate gap: the server needs at least one of request and response, and
  // one is enough to show the shape.
  const OMITTED_ON_PURPOSE: Record<string, string[]> = {
    headers: ["request"],
  };
  it.each(TYPES)(
    "%s: shows every top-level key its struct declares",
    (type) => {
      const parsed = JSON.parse(CONFIG_EXAMPLES[type as never]) as object;
      const missing = [...fieldsOf(STRUCT_OF_TYPE.get(type)!).keys()]
        .filter((key) => !(key in parsed))
        .sort();
      expect(missing).toEqual(OMITTED_ON_PURPOSE[type] ?? []);
    },
  );

  // The one example that is NOT a working config, on purpose. Belune hashes
  // nothing: hashed_password is passed straight to Caddy. A plausible fake hash
  // would pass the API's non-empty check and then silently never authenticate,
  // so this is empty and the server rejects it at once with "username and
  // hashed_password are required" (features.go). If this test fails because
  // someone filled the strings in, that is the bug being guarded.
  it("basic_auth is empty on purpose, and so is the only example the server rejects", () => {
    expect(JSON.parse(CONFIG_EXAMPLES.basic_auth)).toEqual({
      username: "",
      hashed_password: "",
    });
  });

  it("every other example is a working config, not a template with holes", () => {
    const hollow = (value: unknown): boolean =>
      value === "" ||
      (Array.isArray(value) && (value.length === 0 || value.some(hollow))) ||
      (typeof value === "object" &&
        value !== null &&
        !Array.isArray(value) &&
        (Object.keys(value).length === 0 || Object.values(value).some(hollow)));
    for (const type of TYPES.filter((t) => t !== "basic_auth")) {
      expect(hollow(JSON.parse(CONFIG_EXAMPLES[type as never])), type).toBe(
        false,
      );
    }
  });

  it("exampleFor knows only the real types", () => {
    expect(exampleFor("rate_limit")).toBe(CONFIG_EXAMPLES.rate_limit);
    expect(exampleFor("")).toBeUndefined();
    expect(exampleFor("nope")).toBeUndefined();
    // Own properties only: "toString" is on every object.
    expect(exampleFor("toString")).toBeUndefined();
  });
});

describe("configForType", () => {
  const edited = [
    ["a changed value", '{"window":"5s","max_events":3}'],
    ["a partly typed value", '{"from":"/a"'],
    ["an example with a trailing newline", `${CONFIG_EXAMPLES.rate_limit}\n`],
    ["an example with one more space", `${CONFIG_EXAMPLES.headers} `],
    [
      "an example with one character changed",
      '{"window":"2s","max_events":10}',
    ],
  ];

  describe("replaces a value the person has not touched", () => {
    it.each([
      ["empty", ""],
      ["the bare {} it starts as", "{}"],
    ])("%s", (_name, current) => {
      for (const type of TYPES)
        expect(configForType(current, type)).toBe(
          CONFIG_EXAMPLES[type as never],
        );
    });

    // The case that only exists once every type has an example: pick Rate
    // Limit, change your mind, pick Basic Auth. The field is no longer "{}", yet
    // nothing in it is the person's.
    it("one type's example when another type is picked, for every pair", () => {
      for (const from of TYPES)
        for (const to of TYPES)
          expect(
            configForType(CONFIG_EXAMPLES[from as never], to),
            `${from} → ${to}`,
          ).toBe(CONFIG_EXAMPLES[to as never]);
    });
  });

  describe("keeps what the person typed", () => {
    it.each(edited)("%s", (_name, current) => {
      for (const type of TYPES)
        expect(configForType(current, type)).toBe(current);
    });

    it("whatever type is picked, including none", () => {
      expect(configForType('{"a":1}', "")).toBe('{"a":1}');
      expect(configForType('{"a":1}', "not-a-type")).toBe('{"a":1}');
    });
  });

  it("falls back to {} for a type with no example, not to the previous one", () => {
    expect(configForType(CONFIG_EXAMPLES.rate_limit, "")).toBe("{}");
    expect(configForType("", "not-a-type")).toBe("{}");
  });
});
