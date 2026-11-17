import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import { LogView } from "./log-view";
import type { LogEntry } from "./parse";

const entries: LogEntry[] = [
  {
    id: "1",
    level: "error",
    stream: "stderr",
    message: "a line long enough that it would overflow a narrow pane",
    recordedAt: "2026-10-09T06:00:00.000Z",
  },
  { id: "2", level: "info", stream: "stdout", message: "short" },
];

/** The scroll-content box: the `pre`'s only element child. */
function scrollContent(container: HTMLElement): HTMLElement {
  const pre = container.querySelector("pre");
  expect(pre, "LogView should render a <pre> scroller").toBeTruthy();
  const box = pre!.firstElementChild as HTMLElement | null;
  expect(box, "the rows should sit inside a scroll-content box").toBeTruthy();
  return box!;
}

// ⚠️ These assert the CLASS, not the layout, and the difference matters.
// jsdom has no layout engine, so it cannot tell you that a background stops
// short of the text — which is the actual bug. The widths behind this were
// measured in a real browser: without the box every row took the scroller's
// VISIBLE width (1324px) while the widest line ran to 1418px, leaving 104px of
// text sitting on bare terminal background with its level colour and hover
// gone. With it, every row is 1418px.
//
// So what is guarded here is the one thing jsdom can see and a reader can get
// wrong: that the box exists, and that its width rule flips with `wrap`.
describe("LogView scroll-content box", () => {
  it("sizes the rows to the widest line when not wrapping", () => {
    const { container } = render(<LogView entries={entries} />);
    const box = scrollContent(container);

    // w-max = width: max-content → as wide as the widest row's content, so
    // every row stretches to it and the backgrounds span the scrollable width.
    expect(box.className).toContain("w-max");
    expect(box.className).toContain("min-w-full");
  });

  it("sizes the rows to the pane when wrapping", () => {
    const { container } = render(<LogView entries={entries} wrap />);
    const box = scrollContent(container);

    // ⛔ Not w-max. `whitespace-pre-wrap` inside a max-content box lays out
    // against the longest unbroken line and never wraps, so the Wrap toggle
    // would silently do nothing. Verified in a browser: with w-full the pane
    // has zero horizontal overflow and long rows occupy two lines.
    expect(box.className).toContain("w-full");
    expect(box.className).not.toContain("w-max");
  });

  it("keeps every row inside the box, so none can escape the backgrounds", () => {
    const { container } = render(<LogView entries={entries} />);
    const pre = container.querySelector("pre")!;
    const box = scrollContent(container);

    expect(pre.children).toHaveLength(1);
    expect(box.children).toHaveLength(entries.length);
  });
});

// The source column feeds the hanging indent, or wrapped continuation lines misalign. The
// numbers are the existing rule (timestamp 20, level 10) plus the column and its space.
describe("LogView source column", () => {
  const withSource: LogEntry[] = [
    {
      id: "1",
      level: "info",
      message: "wrapped line",
      recordedAt: "2026-10-09T06:00:00.000Z",
      source: "web-0",
    },
    {
      id: "2",
      level: "info",
      message: "another",
      recordedAt: "2026-10-09T06:00:01.000Z",
    },
  ];
  const rows = (c: HTMLElement) =>
    Array.from(
      c.querySelector("pre")!.firstElementChild!.children,
    ) as HTMLElement[];

  it("renders no column, and leaves the indent alone, by default", () => {
    const { container } = render(
      <LogView entries={withSource} wrap showTimestamp showLevel />,
    );
    expect(container.textContent).not.toContain("web-0");
    expect(rows(container)[0].style.paddingLeft).toBe("30ch");
  });

  it("adds the column width and its space to the hanging indent", () => {
    const { container } = render(
      <LogView
        entries={withSource}
        wrap
        showTimestamp
        showLevel
        sourceWidth={8}
      />,
    );
    expect(rows(container)[0].style.paddingLeft).toBe("39ch"); // 20 + (8 + 1) + 10
    expect(rows(container)[0].style.textIndent).toBe("-39ch");
  });

  it("pads a line with no source, so the message column stays aligned", () => {
    const { container } = render(
      <LogView entries={withSource} showTimestamp showLevel sourceWidth={8} />,
    );
    const [a, b] = rows(container).map((r) => r.textContent ?? "");
    expect(a.indexOf("[Info]")).toBe(b.indexOf("[Info]"));
    expect(a).toContain("web-0    ");
  });

  it("truncates a source longer than the column rather than pushing the message out", () => {
    const long: LogEntry[] = [
      { ...withSource[0], source: "worker-12 · 5726bda2 (old)" },
    ];
    const { container } = render(
      <LogView entries={long} showLevel sourceWidth={10} />,
    );
    expect(container.textContent).toContain("worker-12 ");
    expect(container.textContent).not.toContain("5726bda2");
  });
});
