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

  it("is described by the element it points at", async () => {
    const { container } = render(
      <>
        <p id="hint">Use caddy hash-password.</p>
        <CodeEditor
          language="json"
          value="{}"
          onChange={noop}
          aria-label="Config"
          aria-describedby="hint"
        />
      </>,
    );
    await mounted(container);
    expect(
      container.querySelector(".cm-content")?.getAttribute("aria-describedby"),
    ).toBe("hint");
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

describe("onBlur", () => {
  it("reports the editor losing focus", async () => {
    const onBlur = vi.fn();
    const { container } = render(
      <CodeEditor language="plain" value="x" onChange={noop} onBlur={onBlur} />,
    );
    const view = await mounted(container);

    fireEvent.blur(view.contentDOM);

    expect(onBlur).toHaveBeenCalledTimes(1);
  });

  it("calls the handler it has now, not the one it mounted with", async () => {
    const first = vi.fn();
    const second = vi.fn();
    const { container, rerender } = render(
      <CodeEditor language="plain" value="x" onChange={noop} onBlur={first} />,
    );
    const view = await mounted(container);

    rerender(
      <CodeEditor language="plain" value="x" onChange={noop} onBlur={second} />,
    );
    fireEvent.blur(view.contentDOM);

    expect(first).not.toHaveBeenCalled();
    expect(second).toHaveBeenCalledTimes(1);
  });

  it("is optional", async () => {
    const { container } = render(
      <CodeEditor language="plain" value="x" onChange={noop} />,
    );
    const view = await mounted(container);
    expect(() => fireEvent.blur(view.contentDOM)).not.toThrow();
  });
});

// A mounted file is arbitrary: nginx.conf, ini, a shell script. "plain" is no
// parser, and it must not offer opinions about a format it does not know.
describe("the plain language", () => {
  it("highlights nothing, where JSON highlights keys and strings", async () => {
    // Syntax spans only: a matched bracket pair is marked in both languages.
    const styled = (container: HTMLElement) =>
      container.querySelectorAll(
        ".cm-line span:not(.cm-matchingBracket):not(.cm-nonmatchingBracket)",
      ).length;
    const json = render(
      <CodeEditor language="json" value='{"a": "b"}' onChange={noop} />,
    );
    await mounted(json.container);
    expect(styled(json.container)).toBeGreaterThan(0);
    json.unmount();

    const plain = render(
      <CodeEditor language="plain" value='{"a": "b"}' onChange={noop} />,
    );
    await mounted(plain.container);
    expect(styled(plain.container)).toBe(0);
  });

  // An unmatched bracket is a fact about structure in JSON, which JSON.parse
  // rejects too. In a file with no grammar it is just a character (a shell
  // `case x)`, a smiley in a comment), and tinting it would be a verdict the
  // editor has no basis for.
  it("does not flag an unmatched bracket, and still shows a match", async () => {
    const at = (view: EditorView, anchor: number) =>
      view.dispatch({ selection: { anchor } });

    const plain = render(
      <CodeEditor language="plain" value="(oops" onChange={noop} />,
    );
    const plainView = await mounted(plain.container);
    at(plainView, 1);
    expect(plain.container.querySelector(".cm-nonmatchingBracket")).toBeNull();
    plain.unmount();

    const matched = render(
      <CodeEditor language="plain" value="(ok)" onChange={noop} />,
    );
    const matchedView = await mounted(matched.container);
    at(matchedView, 1);
    expect(
      matched.container.querySelectorAll(".cm-matchingBracket"),
    ).toHaveLength(2);
    matched.unmount();

    // JSON keeps the default, so the difference above is the plain path's own.
    const json = render(
      <CodeEditor language="json" value='{"a":' onChange={noop} />,
    );
    const jsonView = await mounted(json.container);
    at(jsonView, 1);
    expect(
      json.container.querySelector(".cm-nonmatchingBracket"),
    ).not.toBeNull();
  });

  it("shows its placeholder like any other language", async () => {
    const { container } = render(
      <CodeEditor
        language="plain"
        value=""
        onChange={noop}
        placeholder="file contents..."
      />,
    );
    await mounted(container);
    expect(container.querySelector(".cm-placeholder")?.textContent).toBe(
      "file contents...",
    );
  });
});

describe("maxHeight and text size", () => {
  const host = (container: HTMLElement) =>
    container.querySelector<HTMLElement>("[data-slot=code-editor]")!;

  it("takes the height cap from the caller, and leaves the default alone without one", async () => {
    const capped = render(
      <CodeEditor
        language="plain"
        value="x"
        onChange={noop}
        maxHeight="32rem"
      />,
    );
    await mounted(capped.container);
    expect(
      host(capped.container).style.getPropertyValue("--code-editor-max-h"),
    ).toBe("32rem");
    capped.unmount();

    const plain = render(
      <CodeEditor language="plain" value="x" onChange={noop} />,
    );
    await mounted(plain.container);
    expect(
      host(plain.container).style.getPropertyValue("--code-editor-max-h"),
    ).toBe("");
  });

  // This field is text-sm today and the primitive defaults to text-xs. The
  // caller's class lands after the default in cn(), so it should win: checked,
  // not assumed, because a naive switch would silently shrink every mounted file.
  it("lets the caller's text size win over the default", async () => {
    const small = render(
      <CodeEditor
        language="plain"
        value="x"
        onChange={noop}
        className="text-sm"
      />,
    );
    await mounted(small.container);
    const classes = host(small.container).classList;
    expect(classes.contains("text-sm")).toBe(true);
    expect(classes.contains("text-xs")).toBe(false);
    small.unmount();

    const dflt = render(
      <CodeEditor language="plain" value="x" onChange={noop} />,
    );
    await mounted(dflt.container);
    expect(host(dflt.container).classList.contains("text-xs")).toBe(true);
  });
});

// Opening a file must not rewrite it. CodeMirror normalises line endings the way
// a <textarea> does, but only when the person edits: a value it merely displays
// must not be reported back as a change.
describe("a value the editor only displays", () => {
  it("is not reported as a change, even when the editor normalises it", async () => {
    const onChange = vi.fn();
    const crlf = "server {\r\n  listen 80;\r\n}\r\n";
    const { container } = render(
      <CodeEditor language="plain" value={crlf} onChange={onChange} />,
    );
    const view = await mounted(container);

    expect(view.state.doc.toString()).toBe("server {\n  listen 80;\n}\n");
    expect(onChange).not.toHaveBeenCalled();
  });
});
