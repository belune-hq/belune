import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { EditorView } from "@codemirror/view";
import { toast } from "sonner";
import type { FileMount } from "@/lib/types";
import { FileMountsSection } from "@/components/applications/file-mounts-section";

const mocks = vi.hoisted(() => ({
  mounts: [] as unknown[],
  create: vi.fn(),
  update: vi.fn(),
  remove: vi.fn(),
  reveal: vi.fn(),
}));

vi.mock("sonner", () => ({
  toast: Object.assign(vi.fn(), { error: vi.fn(), promise: vi.fn() }),
}));
vi.mock("@/lib/hooks/use-file-mounts", () => ({
  useFileMounts: () => ({ data: mocks.mounts, isLoading: false }),
  useCreateFileMount: () => ({ mutateAsync: mocks.create, isPending: false }),
  useUpdateFileMount: () => ({ mutateAsync: mocks.update, isPending: false }),
  useDeleteFileMount: () => ({ mutateAsync: mocks.remove, isPending: false }),
  useRevealFileMount: () => ({ mutateAsync: mocks.reveal, isPending: false }),
}));

// jsdom has no layout engine and CodeMirror measures; these only keep its
// measure pass from throwing. Nothing here says how the editor LOOKS.
beforeAll(() => {
  Range.prototype.getClientRects = () =>
    [] as unknown as ReturnType<Range["getClientRects"]>;
  Range.prototype.getBoundingClientRect = () => ({}) as DOMRect;
});

beforeEach(() => {
  vi.clearAllMocks();
  mocks.mounts = [];
  mocks.create.mockResolvedValue({});
  mocks.update.mockResolvedValue({});
  // The form chains .then(onClose) onto the mutation; sonner would also swallow
  // a rejection, and the mock must too.
  vi.mocked(toast.promise).mockImplementation(((p: Promise<unknown>) => {
    p.catch(() => {});
    return p;
  }) as never);
});

const HIDDEN = "•••••••• hidden — reveal to edit, or type to replace";
const EMPTY = "file contents...";

function mount(overrides: Partial<FileMount> = {}): FileMount {
  return {
    id: "fm-1",
    mount_path: "/etc/app/config.conf",
    content: "",
    is_secret: false,
    file_mode: "0644",
    content_masked: false,
    created_at: "2026-10-01T00:00:00Z",
    ...overrides,
  };
}

function secretMount() {
  return mount({ is_secret: true, content_masked: true, content: "" });
}

function renderSection() {
  return render(<FileMountsSection projectId="p" applicationId="a" />);
}

function openAdd() {
  fireEvent.click(screen.getByRole("button", { name: "Add File" }));
  return screen.getByRole("dialog");
}

function openEdit() {
  fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  return screen.getByRole("dialog");
}

/** The editor, once its chunk has arrived. */
async function editorIn(dialog: HTMLElement): Promise<EditorView> {
  await waitFor(() =>
    expect(dialog.querySelector(".cm-editor")).not.toBeNull(),
  );
  return EditorView.findFromDOM(
    dialog.querySelector<HTMLElement>(".cm-editor")!,
  )!;
}

// act(): CodeMirror reports an edit from its own event handling, outside React's.
function setContent(view: EditorView, text: string) {
  act(() => {
    view.dispatch({
      changes: { from: 0, to: view.state.doc.length, insert: text },
    });
  });
}

const placeholderIn = (dialog: HTMLElement) =>
  dialog.querySelector(".cm-placeholder")?.textContent;

function submit(dialog: HTMLElement) {
  fireEvent.click(
    within(dialog).getByRole("button", { name: /^(Save|Add File)$/ }),
  );
}

// ⚠️ Order matters in this file: the editor chunk loads once per module, so only
// the FIRST test can see the field before it arrives.
describe("the content field", () => {
  // ⛔ A mounted file is multi-line by definition. `compact` falls back to an
  // <input>, which strips newlines, so with the chunk not yet loaded the first
  // keystroke would flatten someone's nginx.conf. This is the test that stops
  // that being "simplified" later: switch the call to `compact` and it goes red.
  it("is a block field whose fallback keeps a multi-line file's newlines", async () => {
    renderSection();
    const dialog = openAdd();

    const field = screen.getByRole("textbox", { name: "Content" });
    expect(field.tagName).toBe("TEXTAREA");

    const conf = "server {\n  listen 80;\n  root /srv;\n}\n";
    fireEvent.change(field, { target: { value: conf } });
    expect((field as HTMLTextAreaElement).value).toBe(conf);

    fireEvent.change(screen.getByLabelText("Mount path"), {
      target: { value: "/etc/nginx/nginx.conf" },
    });
    submit(dialog);

    await waitFor(() => expect(mocks.create).toHaveBeenCalledTimes(1));
    expect(mocks.create).toHaveBeenCalledWith(
      expect.objectContaining({
        mount_path: "/etc/nginx/nginx.conf",
        content: conf,
      }),
    );
  });

  it("is named by its label, and keeps the field's size and cap", async () => {
    renderSection();
    const dialog = openAdd();
    await editorIn(dialog);

    // Named through aria-labelledby: a <label for> cannot reach a contenteditable.
    expect(screen.getByRole("textbox", { name: "Content" })).toBeTruthy();

    // text-sm is what this field has always been; the primitive defaults to
    // text-xs. The cap is the call site's, and the height stays at min-h-40.
    const host = dialog.querySelector<HTMLElement>("[data-slot=code-editor]")!;
    expect(host.classList.contains("text-sm")).toBe(true);
    expect(host.classList.contains("text-xs")).toBe(false);
    expect(host.classList.contains("min-h-40")).toBe(true);
    expect(host.style.getPropertyValue("--code-editor-max-h")).toBe(
      "min(24rem, 40vh)",
    );
  });

  // A mounted file can be anything, so there is no grammar to offer: JSON
  // highlighting on an nginx.conf would colour keys that are not keys, and flag
  // brackets that are only characters. The language is "plain" and is not guessed
  // from the mount path (a .json path is not special-cased).
  it("offers no highlighting and no bracket verdicts, even for JSON-looking content or a .json path", async () => {
    mocks.mounts = [
      mount({ mount_path: "/etc/app/settings.json", content: '{"a": "b"}' }),
    ];
    renderSection();
    const dialog = openEdit();
    const view = await editorIn(dialog);

    const syntaxSpans = () =>
      dialog.querySelectorAll(
        ".cm-line span:not(.cm-matchingBracket):not(.cm-nonmatchingBracket)",
      ).length;
    expect(syntaxSpans()).toBe(0);

    setContent(view, "case $x in a) echo hi ;; esac\n(");
    act(() => view.dispatch({ selection: { anchor: 28 } }));
    expect(syntaxSpans()).toBe(0);
    expect(dialog.querySelector(".cm-nonmatchingBracket")).toBeNull();
  });

  it("sends what is typed in the editor, newlines and all", async () => {
    renderSection();
    const dialog = openAdd();
    const view = await editorIn(dialog);
    const conf = "[server]\nport = 8080\n";

    setContent(view, conf);
    fireEvent.change(screen.getByLabelText("Mount path"), {
      target: { value: "/etc/app.ini" },
    });
    submit(dialog);

    await waitFor(() => expect(mocks.create).toHaveBeenCalledTimes(1));
    expect(mocks.create).toHaveBeenCalledWith(
      expect.objectContaining({ content: conf }),
    );
  });
});

// The secret state is real behaviour, not decoration: it tells someone the box is
// empty because the value is hidden, not because there is nothing there. It has
// to read correctly through the editor's own placeholder.
describe("a secret mount", () => {
  it("says the box is empty because the value is hidden, and stops saying so once revealed", async () => {
    mocks.mounts = [secretMount()];
    mocks.reveal.mockResolvedValue({ content: "TOKEN=abc\nOTHER=1\n" });
    renderSection();
    const dialog = openEdit();
    const view = await editorIn(dialog);

    expect(placeholderIn(dialog)).toBe(HIDDEN);

    fireEvent.click(
      within(dialog).getByRole("button", { name: "Reveal current content" }),
    );
    // The decrypted content reaches the editor from outside it.
    await waitFor(() =>
      expect(view.state.doc.toString()).toBe("TOKEN=abc\nOTHER=1\n"),
    );
    expect(placeholderIn(dialog)).toBeUndefined();

    // Cleared on purpose after a reveal: now it really is empty, and says so.
    setContent(view, "");
    expect(placeholderIn(dialog)).toBe(EMPTY);
  });

  it("does not say it for a mount that is not a hidden secret", async () => {
    mocks.mounts = [mount({ content: "" })];
    renderSection();
    const dialog = openEdit();
    await editorIn(dialog);
    expect(placeholderIn(dialog)).toBe(EMPTY);
  });

  it("does not say it for a new mount", async () => {
    renderSection();
    const dialog = openAdd();
    await editorIn(dialog);
    expect(placeholderIn(dialog)).toBe(EMPTY);
  });

  // keepSecret: an untouched hidden secret means "keep the stored value", so the
  // content is OMITTED. Anything else sends content.
  it("keeps the stored value when saved untouched: no content is sent", async () => {
    mocks.mounts = [secretMount()];
    renderSection();
    const dialog = openEdit();
    await editorIn(dialog);

    submit(dialog);

    await waitFor(() => expect(mocks.update).toHaveBeenCalledTimes(1));
    const sent = mocks.update.mock.calls[0][0];
    expect(sent).toMatchObject({ fileMountId: "fm-1", is_secret: true });
    expect(sent).not.toHaveProperty("content");
  });

  it("replaces the stored value when something is typed over it", async () => {
    mocks.mounts = [secretMount()];
    renderSection();
    const dialog = openEdit();
    const view = await editorIn(dialog);

    setContent(view, "NEW=1\n");
    submit(dialog);

    await waitFor(() => expect(mocks.update).toHaveBeenCalledTimes(1));
    expect(mocks.update.mock.calls[0][0]).toMatchObject({ content: "NEW=1\n" });
  });

  it("sends the revealed content as it is", async () => {
    mocks.mounts = [secretMount()];
    mocks.reveal.mockResolvedValue({ content: "A=1\nB=2\n" });
    renderSection();
    const dialog = openEdit();
    const view = await editorIn(dialog);
    fireEvent.click(
      within(dialog).getByRole("button", { name: "Reveal current content" }),
    );
    await waitFor(() => expect(view.state.doc.toString()).toBe("A=1\nB=2\n"));

    submit(dialog);
    await waitFor(() => expect(mocks.update).toHaveBeenCalledTimes(1));
    expect(mocks.update.mock.calls[0][0]).toMatchObject({
      content: "A=1\nB=2\n",
    });
  });

  // After a reveal, an empty box means "make it empty", not "keep the stored
  // value": the content must be SENT as "", not omitted like an untouched secret.
  it("sends an empty string, not nothing, when cleared on purpose after a reveal", async () => {
    mocks.mounts = [secretMount()];
    mocks.reveal.mockResolvedValue({ content: "A=1\n" });
    renderSection();
    const dialog = openEdit();
    const view = await editorIn(dialog);
    fireEvent.click(
      within(dialog).getByRole("button", { name: "Reveal current content" }),
    );
    await waitFor(() => expect(view.state.doc.toString()).toBe("A=1\n"));

    setContent(view, "");
    submit(dialog);

    await waitFor(() => expect(mocks.update).toHaveBeenCalledTimes(1));
    expect(mocks.update.mock.calls[0][0]).toHaveProperty("content", "");
  });
});
