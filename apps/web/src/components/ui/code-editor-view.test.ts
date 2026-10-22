import { describe, expect, it } from "vitest";
import { HIGHLIGHT_SPEC, THEME_SPEC } from "@/components/ui/code-editor-view";

// Colours come from tokens, never literals: the app has four accent × mode
// combinations and a hex is right in at most one. This walks everything the
// theme and highlight specs emit.
describe("editor theme", () => {
  function* values(node: unknown): Generator<string> {
    if (typeof node === "string") yield node;
    else if (node && typeof node === "object")
      // `tag` is a lezer Tag, a graph with cycles, and holds no CSS.
      for (const [k, v] of Object.entries(node))
        if (k !== "tag") yield* values(v);
  }
  const all = [...values(THEME_SPEC), ...values(HIGHLIGHT_SPEC)];

  it("emits no literal colours", () => {
    expect(all.length).toBeGreaterThan(10);
    for (const v of all) {
      expect(v).not.toMatch(/#[0-9a-f]{3,8}\b/i);
      expect(v).not.toMatch(/\b(rgb|rgba|hsl|hsla|hwb|lab|lch|oklab|oklch)\(/i);
    }
  });

  it("only uses colour keywords that are not a fixed colour", () => {
    // A bare named colour ("red", "black") is a literal in everything but name.
    const named = /^(red|black|white|silver|gray|grey|blue|green|orange)$/i;
    for (const v of all) expect(v).not.toMatch(named);
  });

  it("draws every colour from a token", () => {
    // --code-editor-max-h is the caller's height cap, not a colour.
    const colours = all.filter(
      (v) => /var\(/.test(v) && !v.includes("--code-editor-max-h"),
    );
    expect(colours.length).toBeGreaterThan(8);
    for (const v of colours) {
      // Every var() is a design token, and every mix is of tokens only.
      for (const name of v.matchAll(/var\((--[a-z-]+)\)/g))
        expect(name[1]).toMatch(
          /^--(foreground|muted-foreground|brand-text|brand-soft|brand-line|status-error-soft|border)$/,
        );
    }
  });
});
