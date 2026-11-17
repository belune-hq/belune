import { expect, it } from "vitest";
import { newClientId } from "./client-id";

it("gives draft rows distinct, non-empty string identities", () => {
  const first = newClientId();
  const second = newClientId();
  expect(typeof first).toBe("string");
  expect(first.length).toBeGreaterThan(0);
  expect(second).not.toBe(first);
});
