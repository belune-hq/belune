import { describe, expect, it } from "vitest";
import type { WafStatus } from "@/lib/types";
import {
  ANOMALY_SUMMARY_RULE,
  categoryLabel,
  describeCategory,
  describeEventVerdict,
  describeRecorder,
  describeClientList,
  describeDetectionUrl,
  describeRequestDetail,
  describeUrlCount,
  detectionHeaders,
  describeWaf,
  eventHost,
  isIpAddress,
  severityTone,
  statusTone,
  WAF_ENABLE_POINTS,
  windowPhrase,
} from "./waf-view";

const status = (over: Partial<WafStatus> = {}): WafStatus => ({
  capability: { module: "http.handlers.waf", state: "available" },
  enabled: false,
  mode: "detection_only",
  state: { active: false, hosts: 0 },
  recorder: {
    since: "2026-10-23T00:00:00Z",
    recorded: 0,
    dropped: 0,
    malformed: 0,
  },
  retention_days: 7,
  max_events: 200000,
  summary: {
    window: "24h",
    window_hours: 24,
    events: 0,
    requests: 0,
    clients: 0,
    total: 0,
    top_rules: [],
    top_clients: [],
  },
  ...over,
});

describe("describeWaf", () => {
  // The test that matters: the three capability answers are three different
  // messages. If `unknown` rendered as `unavailable`, someone whose Caddy was merely
  // restarting would be told to rebuild an image that was fine.
  it("keeps unavailable and unknown apart", () => {
    const unavailable = describeWaf(
      status({ capability: { module: "m", state: "unavailable" } }),
    );
    const unknown = describeWaf(
      status({ capability: { module: "m", state: "unknown" } }),
    );

    expect(unavailable.pill.label).toBe("Not active");
    expect(unavailable.body).toMatch(/does not include the WAF module/);
    expect(unavailable.canEnable).toBe(false);

    expect(unknown.pill.label).toBe("Cannot tell yet");
    expect(unknown.body).toMatch(/could not list/i);
    expect(unknown.body).not.toMatch(/does not include/);
    expect(unknown.canEnable).toBe(true);
  });

  it("does not refuse the switch when it merely could not ask", () => {
    const d = describeWaf(
      status({ capability: { module: "m", state: "unknown" } }),
    );
    expect(d.canEnable).toBe(true);
  });

  it("an available module that is off says so, and says what it does", () => {
    const d = describeWaf(status());
    expect(d.pill).toEqual({ label: "Off", tone: "neutral" });
    expect(d.body).toMatch(/only records/);
    expect(d.canEnable).toBe(true);
  });

  it("reports it running, and that it is detection only", () => {
    const d = describeWaf(
      status({ enabled: true, state: { active: true, hosts: 3 } }),
    );
    expect(d.pill).toEqual({ label: "Detecting", tone: "ready" });
    expect(d.body).toMatch(/3 application hostnames/);
    expect(d.body).toMatch(/nothing is blocked/i);
  });

  it("singular hostname", () => {
    const d = describeWaf(
      status({ enabled: true, state: { active: true, hosts: 1 } }),
    );
    expect(d.body).toMatch(/1 application hostname\b(?!s)/);
  });

  it("a running WAF stays reported as running when the probe cannot answer", () => {
    const d = describeWaf(
      status({
        enabled: true,
        capability: { module: "m", state: "unknown" },
        state: { active: true, hosts: 2, reason: "modules_unknown" },
      }),
    );
    expect(d.pill.label).toBe("Detecting");
    expect(d.notice?.text).toMatch(/cannot list Caddy's modules/);
  });

  it("switched on but waiting for an application", () => {
    const d = describeWaf(
      status({
        enabled: true,
        state: { active: false, hosts: 0, reason: "no_applications" },
      }),
    );
    expect(d.pill.label).toBe("Waiting for an application");
  });

  it("surfaces a refusal from Caddy rather than hiding it", () => {
    const d = describeWaf(
      status({
        enabled: true,
        state: { active: false, hosts: 2, error: "HTTP 500: unknown module" },
      }),
    );
    expect(d.pill).toEqual({ label: "Caddy refused it", tone: "error" });
    expect(d.notice).toEqual({
      tone: "error",
      text: "HTTP 500: unknown module",
    });
  });

  it("switched on, module known to be absent, is not the same as could-not-ask", () => {
    const absent = describeWaf(
      status({
        enabled: true,
        state: { active: false, hosts: 2, reason: "module_absent" },
      }),
    );
    const unknown = describeWaf(
      status({
        enabled: true,
        state: { active: false, hosts: 2, reason: "modules_unknown" },
      }),
    );
    expect(absent.pill.label).toBe("Not active");
    expect(unknown.pill.label).toBe("Cannot verify");
    expect(absent.body).not.toBe(unknown.body);
  });
});

describe("describeRecorder", () => {
  const base = { since: "x", recorded: 5, dropped: 0, malformed: 0 };

  it("says nothing when nothing was lost", () => {
    expect(describeRecorder(base)).toEqual([]);
  });

  it("names every way a detection is lost without a row", () => {
    const w = describeRecorder({
      ...base,
      dropped: 1200,
      malformed: 3,
      last_error: "connection refused",
    });
    expect(w.map((x) => x.tone)).toEqual(["error", "attention", "attention"]);
    expect(w[0].text).toContain("connection refused");
    expect(w[1].text).toMatch(/1,200 events were dropped/);
    expect(w[1].text).toMatch(/floor/);
    expect(w[2].text).toMatch(/3 log lines/);
  });
});

describe("events", () => {
  it("rates severity", () => {
    expect(severityTone("critical")).toBe("error");
    expect(severityTone("emergency")).toBe("error");
    expect(severityTone("warning")).toBe("attention");
    expect(severityTone("notice")).toBe("neutral");
    // The anomaly summary declares no severity; it must not look alarming OR vanish.
    expect(severityTone("unknown")).toBe("neutral");
  });

  it("an event with no host is shown as unknown, never blank", () => {
    expect(eventHost({ host: null })).toBe("unknown host");
    expect(eventHost({ host: "shop.example.com" })).toBe("shop.example.com");
  });

  it("trims the attack- prefix from a category", () => {
    expect(categoryLabel("attack-xss")).toBe("xss");
    expect(categoryLabel("attack-sqli")).toBe("sqli");
    expect(categoryLabel(null)).toBeNull();
  });
});

describe("the points shown before it is turned on", () => {
  // These are the caveats the PR is honest about; the UI must say them too.
  it("covers what an operator would be surprised to learn later", () => {
    const text = WAF_ENABLE_POINTS.join(" ");
    expect(text).toMatch(/Nothing is blocked/);
    expect(text).toMatch(/not to this dashboard/);
    expect(text).toMatch(/128 KiB/);
    // The payload promise is the load-bearing one: the URL and content type ARE
    // now shown, joined from the request log, so this must not be loosened into
    // a claim that nothing about the request is kept.
    expect(text).toMatch(/matched text is never stored/);
    expect(text).toMatch(/request log/);
    expect(text).toMatch(/false positives share/);
  });
});

describe("isIpAddress", () => {
  it("accepts addresses and rejects what the server would", () => {
    for (const ok of ["203.0.113.9", "2001:db8::1", "::1", " 10.0.0.1 "]) {
      expect(isIpAddress(ok), ok).toBe(true);
    }
    for (const bad of [
      "",
      "300.1.1.1",
      "1.2.3",
      "example.com",
      "12345",
      "a:b",
    ]) {
      expect(isIpAddress(bad), bad).toBe(false);
    }
  });
});

describe("describeEventVerdict", () => {
  // The whole point. Coraza reports no severity for the anomaly-score rule, so
  // showing severity verbatim put the one row that states a verdict in the
  // faintest badge on the page, under the word "unknown", while a benign
  // content-type match beside it shouted "critical" in red.
  it("names the anomaly-score rule as the verdict it is", () => {
    const v = describeEventVerdict({
      rule_id: ANOMALY_SUMMARY_RULE,
      severity: "unknown",
    });
    expect(v.label).toBe("Would block");
    expect(v.tone).toBe("error");
    expect(v.title).toMatch(/Nothing was blocked/);
    // The error tone alone is not enough: "critical" carries it too, so the
    // verdict would render identically to the noise it is a verdict about.
    expect(v.strong).toBe(true);
    expect(
      describeEventVerdict({ rule_id: 920420, severity: "critical" }).tone,
    ).toBe(v.tone);
  });

  it("says nothing was blocked, because nothing was", () => {
    const v = describeEventVerdict({
      rule_id: ANOMALY_SUMMARY_RULE,
      severity: "unknown",
    });
    expect(v.title).not.toMatch(/blocked it|was refused/);
  });

  it("passes every other rule's severity through untouched", () => {
    for (const severity of ["critical", "warning", "notice"] as const) {
      const v = describeEventVerdict({ rule_id: 920420, severity });
      expect(v.label).toBe(severity);
      expect(v.tone).toBe(severityTone(severity));
      expect(v.title).toBeNull();
      expect(v.strong).toBe(false);
    }
  });
});

describe("windowPhrase", () => {
  it("reads as part of a sentence for every window", () => {
    expect(`Detected in ${windowPhrase("1h")}`).toBe(
      "Detected in the last hour",
    );
    expect(`No detections in ${windowPhrase("24h")}`).toBe(
      "No detections in the last 24 hours",
    );
    expect(windowPhrase("7d")).toBe("the last 7 days");
    expect(windowPhrase("all")).toBe("everything still kept");
  });
});

describe("the no-applications state", () => {
  const waiting = (total: number) =>
    describeWaf(
      status({
        enabled: true,
        state: { active: false, hosts: 0, reason: "no_applications" },
        summary: { ...status().summary, total },
      }),
    );

  // This copy sat above a table of real detections once the drill application
  // was removed: "It starts as soon as an application has a domain to inspect"
  // reads as though nothing had ever been inspected.
  it("does not claim it has never run when it has recorded something", () => {
    const d = waiting(19);
    expect(d.body).not.toMatch(/It starts as soon as/);
    expect(d.body).toMatch(/recorded earlier is still listed below/);
  });

  it("keeps the original wording when nothing has ever been recorded", () => {
    expect(waiting(0).body).toMatch(/It starts as soon as/);
  });
});

describe("describeRequestDetail", () => {
  const detail = {
    method: "POST",
    path: "/checkout?step=2",
    status: 500,
    content_type: "text/plain",
    user_agent: "curl/8.7.1",
  };

  it("reads as one request line", () => {
    expect(describeRequestDetail(detail).line).toBe("POST /checkout?step=2");
  });

  // The whole point of keeping a header: 920420 fires on a benign text/plain
  // POST, and the content type is the entire difference between that and an
  // attack. If it is not in front of the operator, the row is still a shrug.
  it("puts the content type where a 920420 row can be judged by it", () => {
    expect(describeRequestDetail(detail).title).toContain(
      "Content-Type: text/plain",
    );
  });

  it("says the payload is not stored, so its absence is not read as a gap", () => {
    expect(describeRequestDetail(detail).title).toContain("not stored");
  });

  it("drops a status the access entry could not have meant", () => {
    expect(describeRequestDetail({ ...detail, status: 0 }).status).toBeNull();
    expect(describeRequestDetail({ ...detail, status: 200 }).status).toBe(200);
  });

  it("omits a header that was not sent rather than printing null", () => {
    const bare = describeRequestDetail({
      ...detail,
      content_type: null,
      user_agent: null,
    });
    expect(bare.title).not.toContain("null");
    expect(bare.title).not.toContain("Content-Type");
  });

  it("tones a server error apart from a success", () => {
    expect(statusTone(500)).toBe("error");
    expect(statusTone(403)).toBe("attention");
    expect(statusTone(200)).toBe("neutral");
  });
});

describe("describeUrlCount", () => {
  // A detection stores no URI of its own, so this count comes from the request
  // log and falls to zero once that is swept. A bare "0" beside three requests
  // reads as a bug in the page rather than as a retention window.
  it("names the gap instead of showing a zero", () => {
    const none = describeUrlCount(0, 3);
    expect(none.text).toBe("—");
    expect(none.title).toContain("any of these requests");
    expect(none.title).toContain("retention");
    expect(none.title).not.toContain("No request on record");
  });

  it("says one request, not any of these requests, when there was one", () => {
    expect(describeUrlCount(0, 1).title).toContain("this request");
  });

  // The honest reading of a partial count: some of these requests still have
  // their URL, the rest never will again.
  it("calls a count short of the requests a floor", () => {
    const partial = describeUrlCount(1, 3);
    expect(partial.text).toBe("1");
    expect(partial.title).toContain("At least 1 of 3");
    expect(partial.title).toContain("retention");
    // One of the three IS on record, so the sentence must not open by denying it.
    expect(partial.title).not.toContain("No request on record");
  });

  it("adds nothing to a count that covers every request", () => {
    const full = describeUrlCount(3, 3);
    expect(full.text).toBe("3");
    expect(full.title).toBeNull();
  });
});

describe("describeDetectionUrl", () => {
  const request = {
    method: "POST",
    path: "/checkout?step=2",
    status: 500,
    content_type: "text/plain",
    user_agent: "curl/8.7.1",
  };

  it("joins the host from the detection to the path from the request log", () => {
    const url = describeDetectionUrl({ host: "shop.example.com", request });
    expect(url.prefix).toBe("POST shop.example.com");
    expect(url.path).toBe("/checkout?step=2");
    expect(url.status).toBe(500);
    expect(url.kept).toBe(true);
  });

  // The host outlives the path, so a swept request log still names the
  // application that was targeted rather than leaving the cell blank.
  it("keeps the host when the path is gone", () => {
    const url = describeDetectionUrl({
      host: "shop.example.com",
      request: null,
    });
    expect(url.prefix).toBe("shop.example.com");
    expect(url.path).toBe("");
    expect(url.kept).toBe(false);
    expect(url.title).toContain("retention");
  });

  it("does not render an unattributed detection as blank", () => {
    expect(describeDetectionUrl({ host: null, request: null }).prefix).toBe(
      "unknown host",
    );
  });
});

describe("describeClientList", () => {
  // The list is capped rather than paged, so silence would turn a
  // thousand-client scan into a page that looks like fifty clients.
  it("says nothing when the whole list is on screen", () => {
    expect(describeClientList(12, false, "24h")).toBeNull();
  });

  it("says what is missing and how to reach it", () => {
    const text = describeClientList(50, true, "24h")!;
    expect(text).toContain("50 clients");
    expect(text).toContain("the last 24 hours");
    expect(text).toContain("More matched");
    expect(text).toContain("narrow the period");
  });
});

describe("detectionHeaders", () => {
  const detail = {
    method: "POST",
    path: "/checkout?step=2",
    status: 500,
    content_type: "text/plain",
    user_agent: "curl/8.7.1",
  };

  // Content-Type first: it is what tells a benign text/plain POST from an
  // attack when 920420 fires, so it should not be read second.
  it("leads with the header that decides a 920420 row", () => {
    expect(detectionHeaders(detail).map((h) => h.name)).toEqual([
      "Content-Type",
      "User-Agent",
    ]);
    expect(detectionHeaders(detail)[0].value).toBe("text/plain");
  });

  it("omits a header that was not sent rather than listing it empty", () => {
    expect(
      detectionHeaders({ ...detail, content_type: null }).map((h) => h.name),
    ).toEqual(["User-Agent"]);
    expect(
      detectionHeaders({ ...detail, content_type: null, user_agent: null }),
    ).toEqual([]);
  });

  // A detection outlives its request log, and the panel renders the empty list
  // as a sentence rather than as a header section that looks broken.
  it("has nothing to show for a detection whose request is gone", () => {
    expect(detectionHeaders(null)).toEqual([]);
  });
});

describe("describeCategory", () => {
  it("shows the tag as the rule set names it", () => {
    expect(
      describeCategory({ rule_id: 941100, category: "attack-xss" }),
    ).toEqual({
      label: "xss",
      title: "The rule set's own classification: attack-xss.",
    });
  });

  // The one class whose name misleads: it is about the shape of a request, not
  // its contents, and 920420 reports `critical` exactly like a SQLi match does.
  it("says what the protocol class actually means", () => {
    const c = describeCategory({
      rule_id: 920420,
      category: "attack-protocol",
    });
    expect(c.label).toBe("protocol");
    expect(c.title).toContain("not an attack payload");
    expect(c.title).toContain("920420");
  });

  // A dash in a column reads as missing data in a way an absent inline badge
  // never did, so the empty cell carries the reason.
  it("names the anomaly summary as the reason its cell is empty", () => {
    const c = describeCategory({ rule_id: 949110, category: null });
    expect(c.label).toBeNull();
    expect(c.title).toContain("verdict on the whole request");
  });

  it("falls back to a plain reason for any other uncategorised rule", () => {
    const c = describeCategory({ rule_id: 913100, category: null });
    expect(c.label).toBeNull();
    expect(c.title).toBe("This rule declares no attack class.");
  });
});
