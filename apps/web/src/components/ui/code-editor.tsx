import { Component, lazy, Suspense, type ReactNode } from "react";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

/**
 * "plain" has no grammar: no highlighting, and no opinion on brackets. It is for
 * files whose format Belune does not know (a mounted nginx.conf, an ini, a shell
 * script). It is not inferred from anything: choosing a language from a file's
 * extension needs a mapping and a decision for when the extension lies, which is
 * its own change.
 */
export type CodeEditorLanguage = "json" | "plain";

export interface CodeEditorProps {
  value: string;
  onChange: (value: string) => void;
  language: CodeEditorLanguage;
  placeholder?: string;
  /**
   * Called when the field loses focus, as an <input>'s onBlur is. Form libraries
   * key validation timing on it (@tanstack/react-form's `isBlurred`), so an
   * editor that never reported blur would silently change when errors appear.
   * The editor and the plain fallback both call it, or they would disagree about
   * a form's state.
   */
  onBlur?: () => void;
  /**
   * Where the editor stops growing and scrolls, as a CSS length. Defaults to
   * 12rem, which suits a two-key JSON object and not a config file. Set it at the
   * call site, like the minimum height: the next field wants different numbers.
   * Keep it at or above the minimum height, or the editor stops short of the box.
   */
  maxHeight?: string;
  /**
   * Starts one line tall, like an Input, and falls back to an Input.
   *
   * Only for values that survive losing their line breaks (JSON does). An
   * <input> strips newlines from whatever it is given, so a multi-line value
   * (a PEM, a dotenv file) must not use it: the first keystroke in the
   * fallback would silently rewrite it. Without this the field starts a few
   * lines tall and falls back to a Textarea.
   */
  compact?: boolean;
  className?: string;
  "aria-label"?: string;
  "aria-labelledby"?: string;
  /** Hint or help text for the field; read after its name by a screen reader. */
  "aria-describedby"?: string;
}

// Same text treatment on the editor and on the plain field, so swapping one for
// the other changes nothing the eye can see.
const TEXT = "font-mono text-xs";

// Input and Textarea ship `text-base md:text-sm`, and tailwind-merge treats
// `text-xs` and `md:text-sm` as different variants, so a caller's size loses to
// the md rule from the breakpoint up: the plain field was 14px beside a 12px
// editor on a desktop. Repeating the chosen size at md makes the two match at
// every width. A size the caller gave at md is left alone.
const SIZE = /(?:^|\s)(text-(?:xs|sm|base|lg|xl|2xl|3xl))(?=\s|$)/;
const MD_SIZE = /(?:^|\s)md:text-(?:xs|sm|base|lg|xl|2xl|3xl)(?=\s|$)/;

function plainText(className?: string): string {
  const merged = cn(TEXT, className);
  const size = SIZE.exec(merged)?.[1];
  return size && !MD_SIZE.test(merged) ? cn(merged, `md:${size}`) : merged;
}

// A rejected React.lazy stays rejected for the life of the page, so a failed
// load swaps in a fresh one and the next mount gets a real second attempt. The
// usual cause is an update.sh restart while a tab was open: the tab asks for a
// chunk hash the new build no longer has.
//
// The swap is here, on the import failing, and not in the boundary: the
// boundary also catches an editor that loaded fine and then threw, and
// replacing the shared component for that would remount every healthy editor
// on the page.
let View = lazy(loadView);

function loadView() {
  return import("@/components/ui/code-editor-view").catch((error: unknown) => {
    View = lazy(loadView);
    throw error;
  });
}

/**
 * A code editor (CodeMirror 6) for a form field, with its value held by the
 * caller exactly as it would be for an <input>.
 *
 * The editor is a separate chunk, fetched when the first one renders. Until it
 * arrives — and for good, if it never does — the field is a plain <input> or
 * <textarea> on the same value, so the form is usable throughout. That matters
 * most for the fields people reach for when something is already broken.
 *
 * It does not validate. It has no opinion on what a value means; the caller's
 * parsing and the server's checks remain the only authority.
 */
export function CodeEditor({ compact, className, ...props }: CodeEditorProps) {
  const plain = (
    <PlainField compact={compact} className={className} {...props} />
  );
  return (
    <EditorBoundary fallback={plain}>
      <Suspense fallback={plain}>
        <View compact={compact} className={cn(TEXT, className)} {...props} />
      </Suspense>
    </EditorBoundary>
  );
}

function PlainField({
  value,
  onChange,
  onBlur,
  maxHeight,
  placeholder,
  compact,
  className,
  "aria-label": ariaLabel,
  "aria-labelledby": ariaLabelledBy,
  "aria-describedby": ariaDescribedBy,
}: Omit<CodeEditorProps, "language">) {
  const shared = {
    value,
    placeholder,
    spellCheck: false,
    className: plainText(className),
    "aria-label": ariaLabel,
    "aria-labelledby": ariaLabelledBy,
    "aria-describedby": ariaDescribedBy,
    onBlur: () => onBlur?.(),
  };
  return compact ? (
    <Input {...shared} onChange={(e) => onChange(e.target.value)} />
  ) : (
    // The cap applies here too: a Textarea grows with its content, and without
    // one a long file would push the field, and the dialog around it, off screen.
    <Textarea
      {...shared}
      style={maxHeight ? { maxHeight } : undefined}
      onChange={(e) => onChange(e.target.value)}
    />
  );
}

/**
 * Catches the chunk failing to load and anything the editor throws after it
 * has: either way the field falls back to the plain control rather than
 * disappearing. Has to be a class — there is no hook for error boundaries.
 */
class EditorBoundary extends Component<
  { fallback: ReactNode; children: ReactNode },
  { failed: boolean }
> {
  state = { failed: false };

  static getDerivedStateFromError() {
    return { failed: true };
  }

  componentDidCatch(error: unknown) {
    console.warn("Code editor unavailable, using a plain field.", error);
  }

  render() {
    return this.state.failed ? this.props.fallback : this.props.children;
  }
}
