import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { CodeEditor } from "@/components/ui/code-editor";

// The guardrail this file exists for: the field must keep working when the
// editor does not. It is the field someone uses to fix a broken route, and an
// update.sh restart while a tab is open is enough to 404 the editor's chunk.
//
// Its own file because the editor chunk is loaded once per module instance:
// here the load has to FAIL, and nothing else in the suite may have succeeded.

// vitest caches a mock factory's SUCCESS but not its failure, so this file runs
// its failing scenarios first and lets the real module through last. Order the
// tests the other way round and the retry test would be reading a cached module.
const control = vi.hoisted(() => ({ mode: "fail" as "fail" | "ok" }));

vi.mock("@/components/ui/code-editor-view", async (importOriginal) => {
  if (control.mode === "fail")
    throw new Error("Failed to fetch dynamically imported module");
  return importOriginal();
});

beforeAll(() => {
  const rect = {
    x: 0,
    y: 0,
    top: 0,
    left: 0,
    right: 0,
    bottom: 0,
    width: 0,
    height: 0,
  };
  Range.prototype.getClientRects = () =>
    [] as unknown as ReturnType<Range["getClientRects"]>;
  Range.prototype.getBoundingClientRect = () => rect as DOMRect;
});

afterEach(() => {
  vi.restoreAllMocks();
});

function silence() {
  // React reports a caught render error with console.error, and the boundary
  // warns once: both are expected here.
  vi.spyOn(console, "error").mockImplementation(() => {});
  return vi.spyOn(console, "warn").mockImplementation(() => {});
}

describe("CodeEditor when the editor is unavailable", () => {
  it("keeps a working <input> when the chunk fails to load", async () => {
    const warn = silence();
    const onChange = vi.fn();
    control.mode = "fail";
    const { container } = render(
      <CodeEditor
        compact
        language="json"
        value='{"window":"1s"}'
        onChange={onChange}
        aria-label="Config"
        aria-describedby="hint"
      />,
    );

    // Wait for the failure to have actually happened and been handled, not
    // just for the first render: the loading fallback is also an <input>.
    await waitFor(() => expect(warn).toHaveBeenCalled());

    const input = screen.getByRole("textbox", { name: "Config" });
    expect(input.tagName).toBe("INPUT");
    expect(input.getAttribute("aria-describedby")).toBe("hint");
    expect(container.querySelector(".cm-editor")).toBeNull();
    // Still holds the value, and typing still reaches the caller.
    expect(input).toHaveProperty("value", '{"window":"1s"}');
    fireEvent.change(input, { target: { value: '{"window":"2s"}' } });
    expect(onChange).toHaveBeenCalledWith('{"window":"2s"}');
  });

  it("falls back to a <textarea> for a field that is not compact", async () => {
    // An <input> would strip the newlines from a multi-line value, and the
    // first keystroke would rewrite it. A block field must never get one.
    const warn = silence();
    control.mode = "fail";
    const value = "line one\nline two";
    render(
      <CodeEditor
        language="json"
        value={value}
        onChange={() => {}}
        aria-label="Body"
      />,
    );
    await waitFor(() => expect(warn).toHaveBeenCalled());

    const field = screen.getByRole("textbox", { name: "Body" });
    expect(field.tagName).toBe("TEXTAREA");
    expect(field).toHaveProperty("value", value);
  });

  // Input and Textarea ship `text-base md:text-sm`, and tailwind-merge keeps a
  // caller's `text-xs` apart from `md:text-sm`, so the plain field was 14px beside
  // a 12px editor from the md breakpoint up. Found by measuring in a browser.
  it("matches the editor's text size above the md breakpoint too", async () => {
    const warn = silence();
    control.mode = "fail";
    const sizes = async (className?: string) => {
      const { unmount } = render(
        <CodeEditor
          language="json"
          value="x"
          onChange={() => {}}
          className={className}
          aria-label="Body"
        />,
      );
      await waitFor(() => expect(warn).toHaveBeenCalled());
      const classes = [...screen.getByRole("textbox").classList];
      unmount();
      return classes;
    };

    const dflt = await sizes();
    expect(dflt).toContain("text-xs");
    expect(dflt).toContain("md:text-xs");
    expect(dflt).not.toContain("md:text-sm");

    const small = await sizes("text-sm");
    expect(small).toContain("text-sm");
    expect(small).toContain("md:text-sm");
    expect(small).not.toContain("text-xs");

    // A size the caller chose for md is theirs.
    const own = await sizes("text-sm md:text-lg");
    expect(own).toContain("md:text-lg");
    expect(own).not.toContain("md:text-sm");
  });

  it("tries again on the next mount, instead of failing until a page reload", async () => {
    // A rejected React.lazy stays rejected for good, so without the swap in the
    // boundary a transient failure would cost the editor for the rest of the
    // session.
    const warn = silence();
    control.mode = "fail";
    const first = render(
      <CodeEditor
        compact
        language="json"
        value="{}"
        onChange={() => {}}
        aria-label="Config"
      />,
    );
    await waitFor(() => expect(warn).toHaveBeenCalled());
    expect(first.container.querySelector(".cm-editor")).toBeNull();
    first.unmount();

    control.mode = "ok";
    const second = render(
      <CodeEditor
        compact
        language="json"
        value="{}"
        onChange={() => {}}
        aria-label="Config"
      />,
    );
    await waitFor(() =>
      expect(second.container.querySelector(".cm-editor")).not.toBeNull(),
    );
  });

  it("falls back when the editor throws after it has loaded", async () => {
    // The real module is cached by now (see above). An unsupported language
    // makes the real view throw while it mounts, which is the failure being
    // guarded: something inside the editor going wrong AFTER the chunk arrived.
    const warn = silence();
    control.mode = "ok";
    render(
      <CodeEditor
        compact
        language={"not-a-language" as "json"}
        value="{}"
        onChange={() => {}}
        aria-label="Config"
      />,
    );
    await waitFor(() => expect(warn).toHaveBeenCalled());
    expect(screen.getByRole("textbox", { name: "Config" }).tagName).toBe(
      "INPUT",
    );
  });
});
