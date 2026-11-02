import { describe, expect, it } from "vitest";
import {
  domainScheme,
  effectiveForceHTTPS,
  unroutedNotice,
} from "./domain-tls";

describe("domain TLS derivation", () => {
  it("ignores a stored force_https when TLS is off", () => {
    expect(effectiveForceHTTPS("off", true)).toBe(false);
  });

  it("honours force_https in every mode that has a listener", () => {
    for (const mode of ["automatic", "custom", "dns_challenge"]) {
      expect(effectiveForceHTTPS(mode, true)).toBe(true);
      expect(effectiveForceHTTPS(mode, false)).toBe(false);
    }
  });

  it("serves an ssl_mode=off domain over http and every other over https", () => {
    expect(domainScheme("off")).toBe("http");
    for (const mode of ["automatic", "custom", "dns_challenge"]) {
      expect(domainScheme(mode)).toBe("https");
    }
  });
});

describe("unroutedNotice", () => {
  it("explains a domain of a stopped application", () => {
    const n = unroutedNotice("auto", "stopped");
    expect(n).toMatch(/is stopped/);
    expect(n).toMatch(/no certificate is requested/);
  });

  it("says nothing for a running application", () => {
    expect(unroutedNotice("auto", "running")).toBeNull();
  });

  it("says nothing when no certificate is wanted", () => {
    expect(unroutedNotice("off", "stopped")).toBeNull();
  });

  it("claims nothing for a status it does not know, or none yet", () => {
    expect(unroutedNotice("auto", "something-new")).toBeNull();
    expect(unroutedNotice("auto", undefined)).toBeNull();
  });

  it("keeps the other not-running states apart", () => {
    expect(unroutedNotice("dns_challenge", "inactive")).toMatch(
      /has not been deployed yet/,
    );
    expect(unroutedNotice("auto", "error")).toMatch(/error state/);
  });
});
