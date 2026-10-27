import { describe, expect, it } from "vitest";
import {
  HOST_METRICS_FIELD,
  HOST_METRICS_PRESETS,
  RETENTION_BOUNDS,
  RETENTION_FIELDS,
  RETENTION_PRESETS,
  type RetentionField,
  type RetentionPreset,
} from "./retention";

function check(fields: RetentionField[], presets: RetentionPreset[]) {
  for (const field of fields) {
    const bounds = RETENTION_BOUNDS[field.key];
    expect(bounds, `no bounds recorded for ${field.key}`).toBeTruthy();
    for (const preset of presets) {
      const n = Number(preset.value);
      expect(
        Number.isInteger(n) && n >= bounds.min && n <= bounds.max,
        `${field.label}: "${preset.label}" (${preset.value}) is outside ${bounds.min}–${bounds.max}, so picking it returns 400`,
      ).toBe(true);
    }
  }
}

describe("retention presets", () => {
  // The bug this exists for: "Forever" was offered as 0, every field rejected
  // it, and picking it failed with a 400 for several releases.
  it("offers nothing the API would refuse", () => {
    check(RETENTION_FIELDS, RETENTION_PRESETS);
    check([HOST_METRICS_FIELD], HOST_METRICS_PRESETS);
  });

  // One list serves three fields whose ceilings differ, so the shared list is
  // only as wide as the narrowest of them.
  it("stays inside the narrowest ceiling it is shared across", () => {
    const narrowest = Math.min(
      ...RETENTION_FIELDS.map((f) => RETENTION_BOUNDS[f.key].max),
    );
    expect(narrowest).toBe(365);
    for (const p of RETENTION_PRESETS) {
      expect(Number(p.value)).toBeLessThanOrEqual(narrowest);
    }
  });

  it("every field has a fallback its own presets could have produced", () => {
    for (const field of RETENTION_FIELDS) {
      const bounds = RETENTION_BOUNDS[field.key];
      const n = Number(field.fallback);
      expect(n).toBeGreaterThanOrEqual(bounds.min);
      expect(n).toBeLessThanOrEqual(bounds.max);
    }
  });
});
