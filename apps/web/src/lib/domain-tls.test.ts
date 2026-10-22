import { describe, expect, it } from "vitest";
import { domainScheme, effectiveForceHTTPS } from "./domain-tls";

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
