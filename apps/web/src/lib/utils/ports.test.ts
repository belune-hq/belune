import { describe, expect, it } from "vitest";
import { resolvePorts } from "./ports";

const http = {
  health_check_type: "http" as const,
  health_check_path: "/up",
};
const d = (hostname: string, container_port: number | null = null) => ({
  hostname,
  path: "/",
  container_port,
});

describe("resolvePorts", () => {
  it("pins the probe to the app port whatever order domains were added in", () => {
    // The motivating case: user -> 3000, admin -> 3001. Without an app port the
    // probe follows the NEWEST domain, so the order decides the outcome.
    const newestAdmin = [
      d("admin.example.com", 3001),
      d("user.example.com", 3000),
    ];
    const newestUser = [...newestAdmin].reverse();

    expect(
      resolvePorts({ container_port: null, ...http }, newestAdmin).healthPort,
    ).toBe(3001);
    expect(
      resolvePorts({ container_port: null, ...http }, newestUser).healthPort,
    ).toBe(3000);

    for (const order of [newestAdmin, newestUser]) {
      const r = resolvePorts({ container_port: 3000, ...http }, order);
      expect(r.healthPort).toBe(3000);
      expect(r.healthSource).toBe("app");
    }
  });

  it("falls back to 8080 and treats zero as unset", () => {
    const r = resolvePorts({ container_port: 0, ...http }, [
      d("a.example.com", 0),
    ]);
    expect(r.healthPort).toBe(8080);
    expect(r.healthSource).toBe("default");
  });

  it("reports no probe unless an HTTP check with a path is configured", () => {
    for (const app of [
      {
        container_port: 3000,
        health_check_type: "none" as const,
        health_check_path: null,
      },
      {
        container_port: 3000,
        health_check_type: "command" as const,
        health_check_path: null,
      },
      {
        container_port: 3000,
        health_check_type: "http" as const,
        health_check_path: null,
      },
    ]) {
      expect(resolvePorts(app, []).healthPort).toBeNull();
    }
  });
});
