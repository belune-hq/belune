import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { toast } from "sonner";
import { z } from "zod";
import { EyeIcon, FileTextIcon, PencilIcon, Trash2Icon } from "lucide-react";
import {
  useFileMounts,
  useCreateFileMount,
  useUpdateFileMount,
  useDeleteFileMount,
  useRevealFileMount,
} from "@/lib/hooks/use-file-mounts";
import type { FileMount } from "@/lib/types";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { CodeEditor } from "@/components/ui/code-editor";
import { IconAction } from "@/components/ui/icon-action";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Field,
  FieldContent,
  FieldError,
  FieldLabel,
} from "@/components/ui/field";
import { fieldError } from "@/lib/utils/field-error";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";

interface Props {
  projectId: string;
  applicationId: string;
}

export function FileMountsSection({ projectId, applicationId }: Props) {
  const { data: mounts, isLoading } = useFileMounts(projectId, applicationId);
  const deleteMount = useDeleteFileMount(projectId, applicationId);

  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<FileMount | null>(null);
  const [removeTarget, setRemoveTarget] = useState<FileMount | null>(null);

  const openAdd = () => {
    setEditing(null);
    setDialogOpen(true);
  };

  const openEdit = (fm: FileMount) => {
    setEditing(fm);
    setDialogOpen(true);
  };

  const submitRemove = () => {
    if (!removeTarget) return;
    toast.promise(
      deleteMount
        .mutateAsync(removeTarget.id)
        .then(() => setRemoveTarget(null)),
      {
        loading: "Removing...",
        success: "File mount removed",
        error: (err) => err.message,
      },
    );
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <FileTextIcon aria-hidden="true" className="size-4" />
          File Mounts
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex items-start justify-between gap-4">
          <p className="text-muted-foreground text-sm">
            Mount config files into the container at a fixed path — content you
            provide here is written read-only on each deploy. Use it for config
            files an app reads at startup.
          </p>
          <Button size="sm" className="shrink-0" onClick={openAdd}>
            Add File
          </Button>
        </div>

        {isLoading ? (
          <div className="space-y-3">
            {[1, 2].map((i) => (
              <Skeleton key={i} className="h-14 w-full" />
            ))}
          </div>
        ) : !mounts || mounts.length === 0 ? (
          <div className="text-muted-foreground flex flex-col items-center gap-2 rounded-lg border border-dashed p-8 text-center text-sm">
            <FileTextIcon aria-hidden="true" className="size-6" />
            <p>
              No file mounts. Add one to inject a config file into this app.
            </p>
          </div>
        ) : (
          <div className="space-y-3">
            {mounts.map((fm) => (
              <div
                key={fm.id}
                className="flex items-center justify-between gap-3 rounded-lg border p-4"
              >
                <div className="flex min-w-0 items-center gap-3">
                  <div className="bg-elev text-text-muted grid size-9 shrink-0 place-items-center rounded-lg">
                    <FileTextIcon aria-hidden="true" className="size-4" />
                  </div>
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="truncate font-mono text-sm">
                        {fm.mount_path}
                      </span>
                      {fm.is_secret && (
                        <Badge variant="secondary" className="shrink-0">
                          Secret
                        </Badge>
                      )}
                    </div>
                    <div className="text-text-faint text-xs">
                      mode {fm.file_mode}
                    </div>
                  </div>
                </div>
                <div className="flex shrink-0 items-center justify-end gap-1">
                  <IconAction label="Edit" onClick={() => openEdit(fm)}>
                    <PencilIcon aria-hidden="true" className="size-4" />
                  </IconAction>
                  <IconAction
                    label="Remove"
                    destructive
                    onClick={() => setRemoveTarget(fm)}
                  >
                    <Trash2Icon aria-hidden="true" className="size-4" />
                  </IconAction>
                </div>
              </div>
            ))}
          </div>
        )}
      </CardContent>

      {/* Add / edit dialog */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent>
          {/* Keyed so the form re-initializes from the current mount each open,
              rather than fighting useForm's own defaultValues-sync effect. */}
          <FileMountForm
            key={`${editing?.id ?? "new"}-${dialogOpen}`}
            projectId={projectId}
            applicationId={applicationId}
            editing={editing}
            onClose={() => setDialogOpen(false)}
          />
        </DialogContent>
      </Dialog>

      {/* Remove dialog */}
      <AlertDialog
        open={removeTarget !== null}
        onOpenChange={(open) => {
          if (!open) setRemoveTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Remove file mount?</AlertDialogTitle>
            <AlertDialogDescription>
              <span className="font-mono">{removeTarget?.mount_path}</span> will
              no longer be mounted once the application is reloaded.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={submitRemove}
              disabled={deleteMount.isPending}
            >
              Remove
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}

function FileMountForm({
  projectId,
  applicationId,
  editing,
  onClose,
}: {
  projectId: string;
  applicationId: string;
  editing: FileMount | null;
  onClose: () => void;
}) {
  const createMount = useCreateFileMount(projectId, applicationId);
  const updateMount = useUpdateFileMount(projectId, applicationId);
  const revealMount = useRevealFileMount(projectId, applicationId);
  const pending = createMount.isPending || updateMount.isPending;

  // For a secret mount, content starts hidden; the user reveals it to edit in
  // place. `revealed` also disambiguates "empty because hidden" (keep stored)
  // from "explicitly cleared to empty" (replace) on save.
  const [revealed, setRevealed] = useState(!editing || !editing.is_secret);

  const form = useForm({
    defaultValues: {
      mountPath: editing?.mount_path ?? "",
      content: editing?.content ?? "", // empty for secrets (masked)
      isSecret: editing?.is_secret ?? false,
      fileMode: editing?.file_mode ?? "",
    },
    onSubmit: ({ value }) => {
      // A hidden (not-yet-revealed) secret means "keep the stored value" —
      // omit content so the backend preserves it. Once revealed, always send
      // content (even if the user cleared it to make an empty file).
      const keepSecret =
        !!editing && editing.is_secret && !revealed && value.content === "";

      const promise = editing
        ? updateMount.mutateAsync({
            fileMountId: editing.id,
            is_secret: value.isSecret,
            file_mode: value.fileMode || undefined,
            ...(keepSecret ? {} : { content: value.content }),
          })
        : createMount.mutateAsync({
            mount_path: value.mountPath,
            content: value.content,
            is_secret: value.isSecret,
            file_mode: value.fileMode || undefined,
          });

      toast.promise(
        promise.then(() => onClose()),
        {
          loading: editing ? "Saving..." : "Creating file mount...",
          success: editing
            ? "File mount saved — reload the application to apply it"
            : "File mount created — reload the application to mount it",
          error: (err) => err.message,
        },
      );
    },
  });

  const reveal = () => {
    if (!editing) return;
    toast.promise(
      revealMount.mutateAsync(editing.id).then((res) => {
        form.setFieldValue("content", res.content);
        setRevealed(true);
      }),
      {
        loading: "Revealing...",
        success: "Content revealed",
        error: (err) => err.message,
      },
    );
  };

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        e.stopPropagation();
        form.handleSubmit();
      }}
      className="space-y-4"
    >
      <DialogHeader>
        <DialogTitle>
          {editing ? "Edit File Mount" : "Add File Mount"}
        </DialogTitle>
        <DialogDescription>
          The file is written into the container read-only on the next deploy.
        </DialogDescription>
      </DialogHeader>
      <div className="space-y-4 py-2">
        <form.Field
          name="mountPath"
          validators={{
            onChange: z
              .string()
              .min(1, "Mount path is required")
              .refine((v) => v.startsWith("/"), "Must be an absolute path"),
          }}
          children={(field) => {
            const error = fieldError(field.state.meta.errors);
            return (
              <Field data-invalid={!!error}>
                <FieldLabel htmlFor="fm-path">Mount path</FieldLabel>
                <Input
                  id="fm-path"
                  placeholder="/etc/app/config.yaml"
                  className="font-mono"
                  value={field.state.value}
                  disabled={!!editing}
                  onBlur={field.handleBlur}
                  onChange={(e) => field.handleChange(e.target.value)}
                  aria-invalid={!!error}
                />
                {error ? (
                  <FieldError>{error}</FieldError>
                ) : (
                  <p className="text-text-faint text-xs">
                    Absolute file path inside the container, e.g.{" "}
                    <code>/etc/nginx/nginx.conf</code>. The parent directory
                    must exist in the image.
                  </p>
                )}
              </Field>
            );
          }}
        />
        <form.Field
          name="content"
          children={(field) => (
            <div className="space-y-1.5">
              <div className="flex items-center justify-between">
                {/* An id, not htmlFor: a <label for> cannot reach the editor's
                    contenteditable, so the editor is named through
                    aria-labelledby instead. */}
                <Label id="fm-content-label">Content</Label>
                {editing && editing.is_secret && !revealed && (
                  <Button
                    type="button"
                    size="sm"
                    variant="ghost"
                    className="h-7 gap-1.5 text-xs"
                    onClick={reveal}
                    disabled={revealMount.isPending}
                  >
                    <EyeIcon aria-hidden="true" className="size-3.5" />
                    {revealMount.isPending
                      ? "Revealing..."
                      : "Reveal current content"}
                  </Button>
                )}
              </div>
              {/* Block, never `compact`: a mounted file is multi-line by
                  definition, and compact falls back to an <input>, which
                  strips newlines. If the editor chunk had not loaded, the
                  first keystroke would flatten someone's nginx.conf.

                  "plain": a mounted file can be anything, so there is no
                  grammar to offer, and the language is deliberately not
                  guessed from the mount path's extension (that needs a
                  mapping, and a decision for when the extension lies).

                  Size and cap live here, not in the primitive. text-sm is what
                  this field has always been (the primitive defaults to
                  text-xs). The textarea this replaced grew without limit in a
                  dialog that cannot scroll, so a long file pushed Save off the
                  screen; the cap is 24rem, or 40% of the viewport on a short
                  one, which keeps the dialog's other rows and its buttons
                  reachable. */}
              <CodeEditor
                language="plain"
                className="min-h-40 text-sm"
                maxHeight="min(24rem, 40vh)"
                // For a hidden secret the box is empty because the value is
                // hidden, not because there is nothing there: that is what this
                // tells someone, and it must survive the move to the editor.
                placeholder={
                  editing && editing.is_secret && !revealed
                    ? "•••••••• hidden — reveal to edit, or type to replace"
                    : "file contents..."
                }
                value={field.state.value}
                onBlur={field.handleBlur}
                onChange={field.handleChange}
                aria-labelledby="fm-content-label"
              />
            </div>
          )}
        />
        <div className="flex items-center justify-between gap-4">
          <form.Field
            name="isSecret"
            children={(field) => (
              <Label className="flex items-center gap-2 text-sm font-normal">
                <Checkbox
                  checked={field.state.value}
                  onCheckedChange={(v) => field.handleChange(v === true)}
                />
                Secret (mask content in the UI)
              </Label>
            )}
          />
          <form.Field
            name="fileMode"
            validators={{
              onChange: z
                .string()
                .refine(
                  (v) => !v || /^[0-7]{3,4}$/.test(v),
                  "3-4 octal digits, e.g. 0644",
                ),
            }}
            children={(field) => {
              const error = fieldError(field.state.meta.errors);
              return (
                <Field
                  orientation="horizontal"
                  data-invalid={!!error}
                  className="gap-2"
                >
                  <FieldLabel htmlFor="fm-mode" className="text-sm">
                    Mode
                  </FieldLabel>
                  <FieldContent className="gap-1">
                    <Input
                      id="fm-mode"
                      className="w-20 font-mono"
                      placeholder="0644"
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(e) => field.handleChange(e.target.value)}
                      aria-invalid={!!error}
                    />
                    {error && <FieldError>{error}</FieldError>}
                  </FieldContent>
                </Field>
              );
            }}
          />
        </div>
      </div>
      <DialogFooter>
        <Button type="button" variant="outline" onClick={onClose}>
          Cancel
        </Button>
        <form.Subscribe
          selector={(s) => [s.values.mountPath] as const}
          children={([mountPath]) => (
            <Button
              type="submit"
              disabled={pending || (!editing && !mountPath.trim())}
            >
              {pending
                ? editing
                  ? "Saving..."
                  : "Creating..."
                : editing
                  ? "Save"
                  : "Add File"}
            </Button>
          )}
        />
      </DialogFooter>
    </form>
  );
}
