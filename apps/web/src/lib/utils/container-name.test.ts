import { describe, expect, it } from "vitest";
import { shortContainerName } from "./container-name";

const at = (container_name: string, idx = 1) => ({ idx, container_name });

describe("shortContainerName", () => {
  it("keeps {type}-{idx} and the suffix", () => {
    expect(
      shortContainerName(
        "web",
        at("test-app-pcr-calculator-786f8690-web-1-db3df3b2"),
      ),
    ).toBe("web-1-db3df3b2");
  });

  // The reason it is not "the last three parts": the type has a dash of its own.
  it("handles a type name containing a dash", () => {
    expect(
      shortContainerName(
        "worker-1",
        at("test-app-pcr-calculator-786f8690-worker-1-2-2f267ca6", 2),
      ),
    ).toBe("worker-1-2-2f267ca6");
  });

  it("has no suffix for a legacy name that is just the slug", () => {
    expect(shortContainerName("web", at("my-app"))).toBe("web-1");
    expect(shortContainerName("worker", at("my-app-worker-2", 2))).toBe(
      "worker-2",
    );
  });

  it("does not mistake a longer or non-hex tail for a suffix", () => {
    expect(shortContainerName("web", at("app-web-1-5726bda2ff"))).toBe("web-1");
    expect(shortContainerName("web", at("app-web-1-zzzzzzzz"))).toBe("web-1");
  });
});
