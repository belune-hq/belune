import { Component, lazy, Suspense, type ReactNode } from "react";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

export type CodeEditorLanguage = "json";

export interface CodeEditorProps {
  value: string;
  onChange: (value: string) => void;
  language: CodeEditorLanguage;
  placeholder?: string;
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
}

// Same text treatment on the editor and on the plain field, so swapping one for
// the other changes nothing the eye can see.
const TEXT = "font-mono text-xs";

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
  placeholder,
  compact,
  className,
  "aria-label": ariaLabel,
  "aria-labelledby": ariaLabelledBy,
}: Omit<CodeEditorProps, "language">) {
  const shared = {
    value,
    placeholder,
    spellCheck: false,
    className: cn(TEXT, className),
    "aria-label": ariaLabel,
    "aria-labelledby": ariaLabelledBy,
  };
  return compact ? (
    <Input {...shared} onChange={(e) => onChange(e.target.value)} />
  ) : (
    <Textarea {...shared} onChange={(e) => onChange(e.target.value)} />
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
