import { describe, expect, it } from "vitest";
import type { WafEvent, WafExclusion } from "@/lib/types";
import {
  describeIgnored,
  draftFor,
  exclusionsNotice,
  inputFor,
  pathOffer,
  ruleRefusal,
  scopeBadge,
  targetOffer,
  validateDraft,
  type ExclusionDraft,
} from "./waf-exclusion-view";

const COLLECTIONS = ["ARGS", "REQUEST_COOKIES", "REQUEST_HEADERS"];

const event = (over: Partial<WafEvent> = {}): WafEvent =>
  ({
    id: 1,
    recorded_at: "2026-10-28T00:00:00Z",
    client_ip: "203.0.113.9",
    rule_id: 942290,
    severity: "critical",
    category: "attack-sqli",
    message: "x",
    host: "shop.example.com",
    application_id: "app-1",
    tx_id: "t",
    request: {
      method: "GET",
      path: "/a/b?q=1",
      status: 200,
      content_type: null,
      user_agent: null,
    },
    matched_target: "REQUEST_COOKIES:ph_x",
    ...over,
  }) as WafEvent;

const draft = (over: Partial<ExclusionDraft> = {}): ExclusionDraft => ({
  shape: "target",
  collection: "ARGS",
  targetName: "q",
  pathPattern: "",
  scope: "install",
  note: "",
  ...over,
});

describe("ruleRefusal", () => {
  it("refuses the verdict class with the reason, not 'out of range'", () => {
    for (const id of [949110, 959100, 980130, 949999, 980001]) {
      expect(ruleRefusal(id), String(id)).toMatch(/verdict, not a signature/);
    }
  });
  it("refuses what is not a detection rule, and allows the ones that are", () => {
    expect(ruleRefusal(901001)).toMatch(/not a request-side detection rule/);
    expect(ruleRefusal(945000)).toMatch(/not a request-side/);
    for (const id of [910000, 930130, 941100, 942290, 944999]) {
      expect(ruleRefusal(id), String(id)).toBeNull();
    }
  });
});

describe("targetOffer", () => {
  it("offers a keyed target the server would accept", () => {
    expect(
      targetOffer("REQUEST_COOKIES:ph_phc_x_posthog", COLLECTIONS),
    ).toEqual({
      available: true,
      collection: "REQUEST_COOKIES",
      name: "ph_phc_x_posthog",
    });
  });

  it("explains a keyless match instead of offering a name that discriminates nothing", () => {
    const o = targetOffer("REQUEST_FILENAME", COLLECTIONS);
    expect(o.available).toBe(false);
    if (!o.available)
      expect(o.reason).toMatch(/as a whole.*no name to exclude.*path/s);
  });

  it("explains a name that could not be written into a rule", () => {
    // The characters that would end a quoted string or an action list.
    for (const name of ["a,b", 'a"b', "a;b", "a b", "a%{x}", "a/b/", "é"]) {
      const o = targetOffer(`ARGS:${name}`, COLLECTIONS);
      expect(o.available, name).toBe(false);
      if (!o.available) expect(o.reason).toMatch(/cannot be written safely/);
    }
  });

  it("does not offer a collection the server would refuse", () => {
    const o = targetOffer("TX:anomaly_score", COLLECTIONS);
    expect(o.available).toBe(false);
    if (!o.available)
      expect(o.reason).toMatch(/not a field an exclusion can name/);
  });

  it("says when Belune never recorded the field", () => {
    const o = targetOffer(null, COLLECTIONS);
    expect(o.available).toBe(false);
    if (!o.available) expect(o.reason).toMatch(/did not record/);
  });

  it("refuses an over-long name", () => {
    expect(targetOffer(`ARGS:${"a".repeat(81)}`, COLLECTIONS).available).toBe(
      false,
    );
    expect(targetOffer(`ARGS:${"a".repeat(80)}`, COLLECTIONS).available).toBe(
      true,
    );
  });
});

describe("pathOffer", () => {
  it("drops the query and the fragment", () => {
    expect(pathOffer(event())).toBe("/a/b");
    expect(
      pathOffer(event({ request: { ...event().request!, path: "/x#frag" } })),
    ).toBe("/x");
  });
  it("offers nothing for a path an exclusion could not carry, or no request", () => {
    expect(
      pathOffer(event({ request: { ...event().request!, path: "/a b" } })),
    ).toBeNull();
    expect(
      pathOffer(event({ request: { ...event().request!, path: "/%7B" } })),
    ).toBeNull();
    expect(pathOffer(event({ request: null }))).toBeNull();
  });
});

describe("draftFor", () => {
  it("starts from the target, narrowed to the application", () => {
    const r = draftFor(event(), COLLECTIONS);
    expect("draft" in r && r.draft).toMatchObject({
      shape: "target",
      collection: "REQUEST_COOKIES",
      targetName: "ph_x",
      scope: "application",
    });
  });

  it("falls back to the path, then to the whole rule, and never to a made-up target", () => {
    const keyless = draftFor(
      event({ matched_target: "REQUEST_FILENAME" }),
      COLLECTIONS,
    );
    expect("draft" in keyless && keyless.draft.shape).toBe("path");
    expect("draft" in keyless && keyless.draft.pathPattern).toBe("/a/b");

    const nothing = draftFor(
      event({ matched_target: null, request: null }),
      COLLECTIONS,
    );
    expect("draft" in nothing && nothing.draft.shape).toBe("rule");
  });

  it("is install-wide only when the detection names no application", () => {
    const r = draftFor(event({ application_id: null }), COLLECTIONS);
    expect("draft" in r && r.draft.scope).toBe("install");
  });

  it("offers nothing at all for the verdict rule", () => {
    const r = draftFor(event({ rule_id: 949110 }), COLLECTIONS);
    expect("refusal" in r && r.refusal).toMatch(/verdict/);
  });
});

describe("validateDraft", () => {
  it("accepts a complete target and path", () => {
    expect(validateDraft(942290, draft())).toBeUndefined();
    expect(
      validateDraft(930130, draft({ shape: "path", pathPattern: "/api/*" })),
    ).toBeUndefined();
  });
  it("needs a note to turn a rule off, and nothing else for the narrow shapes", () => {
    expect(validateDraft(941100, draft({ shape: "rule", note: "  " }))).toMatch(
      /Say why/,
    );
    expect(
      validateDraft(941100, draft({ shape: "rule", note: "our editor" })),
    ).toBeUndefined();
    expect(validateDraft(942290, draft({ note: "" }))).toBeUndefined();
  });
  it("refuses the paths and names the server would", () => {
    expect(
      validateDraft(930130, draft({ shape: "path", pathPattern: "/*" })),
    ).toMatch(/every path/);
    expect(
      validateDraft(930130, draft({ shape: "path", pathPattern: "x" })),
    ).toMatch(/starts with '\/'/);
    expect(
      validateDraft(930130, draft({ shape: "path", pathPattern: '/x"' })),
    ).toBeDefined();
    expect(validateDraft(942290, draft({ targetName: "a,b" }))).toBeDefined();
  });
  it("refuses an over-long note and the verdict rule", () => {
    expect(validateDraft(942290, draft({ note: "n".repeat(501) }))).toMatch(
      /500/,
    );
    expect(validateDraft(949110, draft())).toMatch(/verdict/);
  });
});

describe("inputFor", () => {
  it("sends only what the shape uses", () => {
    expect(
      inputFor(
        event(),
        draft({ collection: "ARGS", targetName: "q", scope: "application" }),
      ),
    ).toEqual({
      rule_id: 942290,
      collection: "ARGS",
      target_name: "q",
      application_id: "app-1",
    });
    expect(
      inputFor(
        event(),
        draft({ shape: "path", pathPattern: "/x", scope: "install" }),
      ),
    ).toEqual({
      rule_id: 942290,
      path_pattern: "/x",
    });
    expect(
      inputFor(
        event(),
        draft({ shape: "rule", scope: "install", note: " why " }),
      ),
    ).toEqual({
      rule_id: 942290,
      note: "why",
    });
  });
  it("cannot scope to an application the detection does not name", () => {
    const i = inputFor(
      event({ application_id: null }),
      draft({ scope: "application" }),
    );
    expect(i.application_id).toBeUndefined();
  });
});

describe("list wording", () => {
  const ex = (over: Partial<WafExclusion> = {}): WafExclusion => ({
    id: 1,
    rule_id: 942290,
    shape: "target",
    collection: "ARGS",
    target_name: "q",
    path_pattern: "",
    scope: "install",
    application_id: null,
    application_name: "",
    hosts: [],
    inert: false,
    note: "",
    created_at: "2026-10-28T00:00:00Z",
    ...over,
  });

  it("says what each shape ignores", () => {
    expect(describeIgnored(ex())).toBe("ARGS:q");
    expect(
      describeIgnored(ex({ shape: "path", path_pattern: "/.env.local" })),
    ).toBe("the path /.env.local");
    expect(describeIgnored(ex({ shape: "path", path_pattern: "/api/*" }))).toBe(
      "paths beginning /api/",
    );
    expect(describeIgnored(ex({ shape: "rule" }))).toBe("the whole rule");
  });

  it("marks the reach: wide in warning, narrow plain, inert muted", () => {
    expect(scopeBadge(ex())).toEqual({
      label: "Every application",
      tone: "warning",
    });
    expect(
      scopeBadge(
        ex({ scope: "application", application_name: "Shop", hosts: ["a"] }),
      ),
    ).toEqual({ label: "Shop", tone: "neutral" });
    expect(
      scopeBadge(
        ex({ scope: "application", application_name: "Shop", inert: true }),
      ).tone,
    ).toBe("muted");
  });

  it("gives silence a visible reason, and says nothing when nothing is silenced", () => {
    expect(exclusionsNotice(0, 0)).toBeNull();
    expect(exclusionsNotice(1, 0)).toBe(
      "1 exclusion in effect. Detections they cover are not recorded.",
    );
    expect(exclusionsNotice(3, 0)).toMatch(/^3 exclusions in effect/);
    expect(exclusionsNotice(3, 1)).toMatch(/1 of them apply to nothing yet/);
  });
});
