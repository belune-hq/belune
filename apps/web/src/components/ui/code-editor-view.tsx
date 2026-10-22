import { useEffect, useRef, type CSSProperties } from "react";
import {
  Annotation,
  Compartment,
  EditorState,
  Transaction,
  type Extension,
} from "@codemirror/state";
import {
  Decoration,
  EditorView,
  drawSelection,
  keymap,
  placeholder as placeholderExtension,
} from "@codemirror/view";
import { defaultKeymap, history, historyKeymap } from "@codemirror/commands";
import {
  HighlightStyle,
  bracketMatching,
  syntaxHighlighting,
  type MatchResult,
} from "@codemirror/language";
import { json } from "@codemirror/lang-json";
import { tags as t } from "@lezer/highlight";
import { cn } from "@/lib/utils";
import type {
  CodeEditorLanguage,
  CodeEditorProps,
} from "@/components/ui/code-editor";

/**
 * The CodeMirror half of <CodeEditor>. Nothing outside code-editor.tsx may
 * import this statically: it is the whole editor bundle, and the point of the
 * split is that the app does not fetch it until a field actually renders one.
 */

// "plain" is no parser at all: a mounted file is arbitrary, and no language pack
// is worth shipping for a format Belune does not know.
const LANGUAGES: Record<CodeEditorLanguage, () => Extension> = {
  json: () => json(),
  plain: () => [],
};

const matchingMark = Decoration.mark({ class: "cm-matchingBracket" });

// With no grammar an unmatched bracket is just a character (a shell `case x)`, a
// smiley in a comment), so only a MATCH is worth showing. JSON keeps the default,
// where an unmatched bracket is a fact about structure that JSON.parse would
// reject too.
function renderMatchOnly(match: MatchResult) {
  if (!match.matched || !match.end) return [];
  return [
    matchingMark.range(match.start.from, match.start.to),
    matchingMark.range(match.end.from, match.end.to),
  ];
}

// Every colour below is a design token, never a literal. The app flips between
// light and dark by redefining CSS variables, not by anything CodeMirror knows
// about, so a hex here would be right in one of the four accent × mode
// combinations and wrong in the other three. CodeMirror therefore stays in its
// own "light" mode permanently, and each rule from its base theme that carries
// a literal (caret, selection, placeholder, bracket match, focus outline) is
// overridden below with the same selector shape, so it ties on specificity and
// the later-mounted theme wins. (`&light` is only available to baseTheme; here
// the plain `&` selects the same editor element.)
export const THEME_SPEC = {
  "&": {
    color: "var(--foreground)",
    backgroundColor: "transparent",
    // Grow with the content, then scroll: a long config should not push the
    // dialog's Add button off the screen. The cap is the caller's (the host sets
    // --code-editor-max-h from the maxHeight prop); 12rem suits a small JSON
    // object and nothing larger.
    maxHeight: "var(--code-editor-max-h, 12rem)",
  },
  // The host element draws the focus ring, like Input does; a second
  // outline from CodeMirror would double it.
  "&.cm-focused": { outline: "none" },
  ".cm-scroller": {
    // Size and family come from the host's `font-mono text-xs`, so the editor
    // and the plain-field fallback cannot drift apart.
    fontFamily: "inherit",
    lineHeight: "inherit",
    overflow: "auto",
  },
  ".cm-content": {
    // index.css turns ligatures off for code/pre because Geist Mono merges
    // sequences like `--` and `<>`, which swallows the characters being typed.
    // CodeMirror's content is a div, so that rule does not reach it.
    fontFeatureSettings: '"ss01", "liga" 0, "calt" 0',
    fontVariantLigatures: "none",
    // Centre one line in the 2rem Input-sized box (minus its 1px borders), so
    // the editor and the fallback Input are the same height and nothing jumps
    // when the chunk arrives. `lh` follows the host's text size.
    padding: "calc((2rem - 2px - 1lh) / 2) 0",
    caretColor: "var(--foreground)",
  },
  ".cm-line": { padding: "0 0.625rem" },
  ".cm-cursor, .cm-dropCursor": { borderLeftColor: "var(--foreground)" },
  // Focused selection is --brand-soft, the same tint index.css gives ::selection
  // everywhere else. A stronger tint (--brand-line) dropped a selected key to
  // 2.9:1; --brand-soft keeps it above 4.5:1 in all four combinations. Unfocused
  // it goes neutral, so a selection left behind does not look like it has focus.
  ".cm-selectionBackground": { background: "var(--border)" },
  "&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground": {
    background: "var(--brand-soft)",
  },
  ".cm-placeholder": { color: "var(--muted-foreground)" },
  "&.cm-focused .cm-matchingBracket": {
    backgroundColor: "var(--brand-soft)",
    outline: "1px solid var(--brand-line)",
  },
  // The one --status token in the editor, and not a verdict: a bracket with no
  // partner is a fact about the text's structure, which JSON.parse would reject
  // too. Only ever shown beside the caret, never for what the config means.
  "&.cm-focused .cm-nonmatchingBracket": {
    backgroundColor: "var(--status-error-soft)",
  },
};

// Colour carries structure here, never validity: a number is not tinted for
// being out of range, and nothing is tinted red. Whether a config is acceptable
// is JSON.parse's call and then the server's, and an editor that coloured
// `"max_events": 0` as fine or as wrong would be a second, unauthoritative
// opinion. That also rules out the --status-* tokens for syntax colours, which
// read as verdicts.
//
// Only tokens that stay legible as text in all four accent × mode combinations
// are used: the brand as text (--brand-text, which exists for exactly this),
// and the foreground ramp. The key and number/keyword tints are mixes of two of
// them rather than new colours, so there is still nothing to define.
//
// Keys are 85% --brand-text, not 100%: that token is tuned against the bare
// page, but the field sits on the lighter `dark:bg-input/30` fill, where
// violet's 4.7:1 becomes 4.29:1, under AA for 12px text. Measured with the
// mix, the worst of the four combinations is 5.38:1.
export const HIGHLIGHT_SPEC = [
  {
    tag: t.propertyName,
    color: "color-mix(in srgb, var(--brand-text) 85%, var(--foreground))",
  },
  { tag: t.string, color: "var(--foreground)" },
  {
    tag: [t.number, t.bool, t.null],
    color: "color-mix(in srgb, var(--brand-text) 60%, var(--foreground))",
  },
  {
    tag: [t.separator, t.brace, t.squareBracket],
    color: "var(--muted-foreground)",
  },
];

const theme = EditorView.theme(THEME_SPEC);
const highlight = syntaxHighlighting(HighlightStyle.define(HIGHLIGHT_SPEC));

// Marks a transaction that came from the `value` prop, so the update listener
// does not hand it straight back to onChange: that would be an echo loop, and
// it would also report a change the user never made.
const fromProps = Annotation.define<boolean>();

// What may change over the editor's life without destroying it (and with it the
// undo history, the selection and focus). The placeholder follows the selected
// feature type, so it really does change.
const settings = new Compartment();

interface Settings {
  language: CodeEditorLanguage;
  placeholder?: string;
  ariaLabel?: string;
  ariaLabelledBy?: string;
  ariaDescribedBy?: string;
}

function settingsExtension(s: Settings): Extension {
  const attributes: Record<string, string> = {};
  if (s.ariaLabel) attributes["aria-label"] = s.ariaLabel;
  if (s.ariaLabelledBy) attributes["aria-labelledby"] = s.ariaLabelledBy;
  if (s.ariaDescribedBy) attributes["aria-describedby"] = s.ariaDescribedBy;
  return [
    LANGUAGES[s.language](),
    bracketMatching(
      s.language === "plain" ? { renderMatch: renderMatchOnly } : {},
    ),
    s.placeholder ? placeholderExtension(s.placeholder) : [],
    // CodeMirror already gives the content role="textbox" and aria-multiline;
    // a <label for> cannot reach a contenteditable, so the name is set here.
    EditorView.contentAttributes.of(attributes),
  ];
}

export type CodeEditorViewProps = Omit<CodeEditorProps, "fallback">;

export default function CodeEditorView({
  value,
  onChange,
  onBlur,
  maxHeight,
  language,
  placeholder,
  compact,
  className,
  "aria-label": ariaLabel,
  "aria-labelledby": ariaLabelledBy,
  "aria-describedby": ariaDescribedBy,
}: CodeEditorViewProps) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  const config: Settings = {
    language,
    placeholder,
    ariaLabel,
    ariaLabelledBy,
    ariaDescribedBy,
  };

  // The mount effect and the update listener outlive any one render, so they
  // read the latest props through here rather than closing over the first.
  const latest = useRef({ value, onChange, onBlur, config });
  useEffect(() => {
    latest.current = { value, onChange, onBlur, config };
  });

  useEffect(() => {
    const parent = host.current;
    if (!parent) return;
    const editor = new EditorView({
      parent,
      state: EditorState.create({
        doc: latest.current.value,
        extensions: [
          history(),
          drawSelection(),
          highlight,
          // Long one-line JSON wraps inside the dialog instead of scrolling
          // sideways out of view.
          EditorView.lineWrapping,
          // Deliberately no Tab binding: indentWithTab would trap keyboard
          // users in the field. Tab moves on to the next control.
          keymap.of([...defaultKeymap, ...historyKeymap]),
          theme,
          // blur, like the plain field's onBlur, reaches the caller: form
          // libraries time their validation on it. Read through `latest`, so a
          // handler that changes between renders is the one that runs.
          EditorView.domEventHandlers({
            blur: () => {
              latest.current.onBlur?.();
            },
          }),
          EditorView.updateListener.of((update) => {
            if (!update.docChanged) return;
            if (update.transactions.some((tr) => tr.annotation(fromProps)))
              return;
            latest.current.onChange(update.state.doc.toString());
          }),
          settings.of(settingsExtension(latest.current.config)),
        ],
      }),
    });
    view.current = editor;
    return () => {
      editor.destroy();
      view.current = null;
    };
  }, []);

  // value → editor, for changes the editor did not make itself (picking a
  // feature type prefills an example; a successful add resets the field).
  useEffect(() => {
    const editor = view.current;
    if (!editor) return;
    const doc = editor.state.doc.toString();
    if (doc === value) return;
    editor.dispatch({
      changes: { from: 0, to: doc.length, insert: value },
      // Not an edit the user made, so Ctrl+Z should not resurrect the text the
      // form just cleared.
      annotations: [fromProps.of(true), Transaction.addToHistory.of(false)],
    });
  }, [value]);

  useEffect(() => {
    view.current?.dispatch({
      effects: settings.reconfigure(
        settingsExtension({
          language,
          placeholder,
          ariaLabel,
          ariaLabelledBy,
          ariaDescribedBy,
        }),
      ),
    });
  }, [language, placeholder, ariaLabel, ariaLabelledBy, ariaDescribedBy]);

  return (
    <div
      ref={host}
      data-slot="code-editor"
      className={cn(
        // grid-cols-[minmax(0,1fr)]: a grid track defaults to min-content, so
        // an unbroken line would widen the track and the dialog with it. The
        // grid also stretches the editor to the host's min height, which is
        // what makes the whole box clickable rather than just its first line.
        "border-input dark:bg-input/30 focus-within:border-ring focus-within:ring-ring/50 grid w-full min-w-0 grid-cols-[minmax(0,1fr)] rounded-lg border bg-transparent transition-colors focus-within:ring-3",
        compact ? "min-h-8" : "min-h-16",
        className,
      )}
      style={
        maxHeight
          ? ({ "--code-editor-max-h": maxHeight } as CSSProperties)
          : undefined
      }
    />
  );
}
