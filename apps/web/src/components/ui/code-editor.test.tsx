import { beforeAll, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { undo } from "@codemirror/commands";
import { EditorView } from "@codemirror/view";
import { CodeEditor } from "@/components/ui/code-editor";

// jsdom has no layout engine, and CodeMirror measures. These stubs only keep
// its measure pass from throwing; nothing here says anything about how the
// editor LOOKS. That is what /dev/theme and the browser smoke drill are for.
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
    toJSON: () => ({}),
  };
  Range.prototype.getClientRects = () =>
    [] as unknown as ReturnType<Range["getClientRects"]>;
  Range.prototype.getBoundingClientRect = () => rect as DOMRect;
});

const noop = () => {};

function editorOf(container: HTMLElement): EditorView {
  const dom = container.querySelector<HTMLElement>(".cm-editor");
  const view = dom && EditorView.findFromDOM(dom);
  if (!view) throw new Error("no CodeMirror editor mounted");
  return view;
}

async function mounted(container: HTMLElement): Promise<EditorView> {
  await waitFor(() =>
    expect(container.querySelector(".cm-editor")).not.toBeNull(),
  );
  return editorOf(container);
}

// ⚠️ Order matters in this file. The editor chunk is loaded once per module
// instance, so only the FIRST render can observe the loading state; everything
// after it gets the editor synchronously.
describe("CodeEditor", () => {
  it("is a usable plain field while the editor loads, then hands over the value", async () => {
    const onChange = vi.fn();
    const { container } = render(
      <CodeEditor
        compact
        language="json"
        value='{"a":1}'
        onChange={onChange}
        aria-label="Config"
      />,
    );

    // Before the chunk resolves: an <input>, already holding the value, and
    // typing in it reaches the caller.
    const input = screen.getByRole("textbox", { name: "Config" });
    expect(input.tagName).toBe("INPUT");
    expect(input).toHaveProperty("value", '{"a":1}');
    fireEvent.change(input, { target: { value: '{"a":2}' } });
    expect(onChange).toHaveBeenCalledWith('{"a":2}');

    // Then the editor replaces it, carrying the caller's value (the caller did
    // not accept the change above, so it is still the original).
    const view = await mounted(container);
    expect(view.state.doc.toString()).toBe('{"a":1}');
    expect(container.querySelector("input")).toBeNull();
  });

  it("reports the editor's own edits to onChange", async () => {
    const onChange = vi.fn();
    const { container } = render(
      <CodeEditor language="json" value="{}" onChange={onChange} />,
    );
    const view = await mounted(container);

    view.dispatch({ changes: { from: 0, to: 2, insert: '{"x":1}' } });
    expect(onChange).toHaveBeenCalledExactlyOnceWith('{"x":1}');
  });

  // The guardrail. The editor has no opinion on what a value means: JSON.parse
  // in the form and then the server's ParseFeatureConfig are the only authority.
  // `max_events: 0` is the case that matters (it is valid JSON that makes a
  // route answer nothing, and the SERVER rejects it); the others are values an
  // editor that "helped" might rewrite, reject or flag.
  it.each([
    ["max_events zero", '{"window":"1s","max_events":0}'],
    ["unterminated", '{"window":'],
    ["not JSON at all", "{ not json"],
    ["empty", ""],
  ])("passes %s through untouched and flags nothing", async (_name, text) => {
    const onChange = vi.fn();
    const { container } = render(
      <CodeEditor language="json" value="{}" onChange={onChange} />,
    );
    const view = await mounted(container);

    view.dispatch({ changes: { from: 0, to: 2, insert: text } });

    expect(view.state.doc.toString()).toBe(text);
    expect(onChange).toHaveBeenLastCalledWith(text);
    // No diagnostics, no lint marks, no invalid state: nothing in the DOM that
    // would read as the editor passing judgement.
    expect(container.querySelector(".cm-diagnostic, .cm-lintRange")).toBeNull();
    expect(container.querySelector("[aria-invalid]")).toBeNull();
  });

  it("takes a changed value from the caller without echoing it back", async () => {
    const onChange = vi.fn();
    const { container, rerender } = render(
      <CodeEditor language="json" value="{}" onChange={onChange} />,
    );
    const view = await mounted(container);

    rerender(
      <CodeEditor
        language="json"
        value='{"window":"1s","max_events":10}'
        onChange={onChange}
      />,
    );

    expect(view.state.doc.toString()).toBe('{"window":"1s","max_events":10}');
    // A prefill is not something the user typed: no onChange (that would be an
    // echo loop), and nothing for Ctrl+Z to undo back to the old text.
    expect(onChange).not.toHaveBeenCalled();
    expect(undo(view)).toBe(false);
  });

  it("highlights JSON structure", async () => {
    const { container } = render(
      <CodeEditor language="json" value='{"window":"1s"}' onChange={noop} />,
    );
    await mounted(container);

    const spanClass = (text: string) =>
      [...container.querySelectorAll(".cm-line span")].find(
        (el) => el.textContent === text,
      )?.className;
    // A key and a string value are styled, and differently from each other.
    expect(spanClass('"window"')).toBeTruthy();
    expect(spanClass('"1s"')).toBeTruthy();
    expect(spanClass('"window"')).not.toBe(spanClass('"1s"'));
  });

  it("follows a changed placeholder and keeps its accessible name", async () => {
    const { container, rerender } = render(
      <>
        <span id="lbl">Config (JSON)</span>
        <CodeEditor
          language="json"
          value=""
          onChange={noop}
          placeholder="first"
          aria-labelledby="lbl"
        />
      </>,
    );
    await mounted(container);
    expect(container.querySelector(".cm-placeholder")?.textContent).toBe(
      "first",
    );
    // contenteditable cannot be reached by <label for>, so this is the only
    // thing that names it to a screen reader.
    expect(screen.getByRole("textbox", { name: "Config (JSON)" })).toBeTruthy();

    rerender(
      <>
        <span id="lbl">Config (JSON)</span>
        <CodeEditor
          language="json"
          value=""
          onChange={noop}
          placeholder="second"
          aria-labelledby="lbl"
        />
      </>,
    );
    expect(container.querySelector(".cm-placeholder")?.textContent).toBe(
      "second",
    );
  });
});
