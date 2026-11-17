export async function copyToClipboard(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard) {
      await navigator.clipboard.writeText(text);
      return true;
    }

    // Plain HTTP installs have no Clipboard API. execCommand still supports
    // copying during a user gesture there; preserve focus and selection.
    const activeElement = document.activeElement;
    const selection = document.getSelection();
    const ranges = selection
      ? Array.from({ length: selection.rangeCount }, (_, i) =>
          selection.getRangeAt(i).cloneRange(),
        )
      : [];
    const inputSelection =
      activeElement instanceof HTMLInputElement ||
      activeElement instanceof HTMLTextAreaElement
        ? {
            start: activeElement.selectionStart,
            end: activeElement.selectionEnd,
            direction: activeElement.selectionDirection,
          }
        : null;
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.readOnly = true;
    textarea.style.position = "fixed";
    textarea.style.left = "-9999px";
    // Keep the fallback inside an open modal's focus trap, if present.
    (
      activeElement?.closest('[role="dialog"], [role="alertdialog"]') ??
      document.body
    ).appendChild(textarea);
    try {
      textarea.select();
      return document.execCommand("copy");
    } finally {
      textarea.remove();
      if (activeElement instanceof HTMLElement) activeElement.focus();
      if (selection) {
        selection.removeAllRanges();
        ranges.forEach((range) => selection.addRange(range));
      }
      if (
        inputSelection?.start != null &&
        inputSelection.end != null &&
        (activeElement instanceof HTMLInputElement ||
          activeElement instanceof HTMLTextAreaElement)
      ) {
        activeElement.setSelectionRange(
          inputSelection.start,
          inputSelection.end,
          inputSelection.direction ?? undefined,
        );
      }
    }
  } catch {
    return false;
  }
}
