import { useAuthStore } from "@/lib/stores/auth";
import { AdoptVolumeDialog } from "./adopt-volume-dialog";
import { Badge } from "@/components/ui/badge";
import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { toast } from "sonner";
import { z } from "zod";
import { HardDriveIcon, InfoIcon, Trash2Icon } from "lucide-react";
import {
  useVolumes,
  useCreateVolume,
  useDeleteVolume,
} from "@/lib/hooks/use-volumes";
import type { ApplicationVolume } from "@/lib/types";
import { formatBytes } from "@/lib/utils/format";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { IconAction } from "@/components/ui/icon-action";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
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
  canDelete: boolean;
}

const SYSTEM_PATHS = new Set(["/", "/tmp", "/etc"]);

export function VolumesSection({ projectId, applicationId, canDelete }: Props) {
  const { data: volumes, isLoading } = useVolumes(projectId, applicationId);
  const createVolume = useCreateVolume(projectId, applicationId);
  const deleteVolume = useDeleteVolume(projectId, applicationId);

  const [addOpen, setAddOpen] = useState(false);
  const [adoptOpen, setAdoptOpen] = useState(false);
  const isAdmin = useAuthStore((s) => s.user?.role === "admin");

  const [removeTarget, setRemoveTarget] = useState<ApplicationVolume | null>(
    null,
  );
  const [deleteData, setDeleteData] = useState(false);

  const addForm = useForm({
    defaultValues: { name: "", mountPath: "" },
    onSubmit: ({ value }) => {
      toast.promise(
        createVolume
          .mutateAsync({ name: value.name, mount_path: value.mountPath })
          .then(() => {
            setAddOpen(false);
            addForm.reset();
          }),
        {
          loading: "Creating volume...",
          success: "Volume created — reload the application to mount it",
          error: (err) => err.message,
        },
      );
    },
  });

  const submitRemove = () => {
    if (!removeTarget) return;
    toast.promise(
      deleteVolume
        .mutateAsync({ volumeId: removeTarget.id, deleteData })
        .then((res) => {
          setRemoveTarget(null);
          setDeleteData(false);
          if (res.warning) toast.warning(res.warning);
        }),
      {
        loading: "Removing volume...",
        success: "Volume removed",
        error: (err) => err.message,
      },
    );
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <HardDriveIcon aria-hidden="true" className="size-4" />
          Volumes
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex items-start justify-between gap-4">
          <p className="text-muted-foreground text-sm">
            Persistent storage mounted into the container. Data survives
            redeploys and restarts.
          </p>
          <div className="flex shrink-0 gap-2">
            {isAdmin && (
              <Button
                size="sm"
                variant="outline"
                onClick={() => setAdoptOpen(true)}
              >
                Adopt Existing Volume
              </Button>
            )}
            <Button
              size="sm"
              className="shrink-0"
              onClick={() => {
                addForm.reset();
                setAddOpen(true);
              }}
            >
              Add Volume
            </Button>
          </div>
        </div>

        <div className="bg-muted/40 text-muted-foreground flex items-start gap-2 rounded-lg border p-3 text-sm">
          <InfoIcon aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
          <span>
            Mounts are attached when the container is created, so a reload is
            enough to apply a change here — no rebuild needed. The badge beside
            the application name says when one is outstanding.
          </span>
        </div>

        {isLoading ? (
          <div className="space-y-3">
            {[1, 2].map((i) => (
              <Skeleton key={i} className="h-16 w-full" />
            ))}
          </div>
        ) : !volumes || volumes.length === 0 ? (
          <div className="text-muted-foreground flex flex-col items-center gap-2 rounded-lg border border-dashed p-10 text-center text-sm">
            <HardDriveIcon aria-hidden="true" className="size-6" />
            <p>
              No volumes yet. Add one to give this application persistent
              storage.
            </p>
          </div>
        ) : (
          <div className="space-y-3">
            {volumes.map((vol) => (
              <div
                key={vol.id}
                className="flex items-center justify-between gap-3 rounded-lg border p-4"
              >
                <div className="flex min-w-0 items-center gap-3">
                  <div className="bg-elev text-text-muted grid size-9 shrink-0 place-items-center rounded-lg">
                    <HardDriveIcon aria-hidden="true" className="size-4" />
                  </div>
                  <div className="min-w-0">
                    <div className="flex items-center gap-2 font-medium">
                      {vol.name}
                      {vol.external_name && (
                        <Badge variant="secondary">Adopted</Badge>
                      )}
                    </div>
                    {vol.external_name && (
                      <p className="text-muted-foreground text-xs">
                        {vol.external_name} · retained when detached
                      </p>
                    )}
                    <div className="text-text-faint truncate font-mono text-sm">
                      {vol.mount_path}
                    </div>
                  </div>
                </div>
                <div className="flex shrink-0 items-center justify-end gap-1">
                  <span className="text-text-faint mr-1 text-sm tabular-nums">
                    {formatBytes(vol.size_bytes)}
                  </span>
                  {canDelete && (
                    <IconAction
                      label="Remove volume"
                      destructive
                      onClick={() => {
                        setDeleteData(false);
                        setRemoveTarget(vol);
                      }}
                    >
                      <Trash2Icon aria-hidden="true" className="size-4" />
                    </IconAction>
                  )}
                </div>
              </div>
            ))}
          </div>
        )}
      </CardContent>

      {isAdmin && (
        <AdoptVolumeDialog
          projectId={projectId}
          applicationId={applicationId}
          open={adoptOpen}
          onOpenChange={setAdoptOpen}
        />
      )}
      {/* Add volume dialog */}
      <Dialog open={addOpen} onOpenChange={setAddOpen}>
        <DialogContent>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              e.stopPropagation();
              addForm.handleSubmit();
            }}
            className="space-y-4"
          >
            <DialogHeader>
              <DialogTitle>Add Volume</DialogTitle>
              <DialogDescription>
                Give the volume a name and the absolute path where it should be
                mounted inside the container.
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4 py-2">
              <addForm.Field
                name="name"
                validators={{
                  onChange: z
                    .string()
                    .min(1, "Name is required")
                    .regex(
                      /^[a-z0-9-]+$/,
                      "Lowercase letters, numbers and hyphens only",
                    ),
                }}
                children={(field) => {
                  const error = fieldError(field.state.meta.errors);
                  return (
                    <Field data-invalid={!!error}>
                      <FieldLabel htmlFor="volume-name">Name</FieldLabel>
                      <Input
                        id="volume-name"
                        placeholder="data"
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        aria-invalid={!!error}
                      />
                      {error ? (
                        <FieldError>{error}</FieldError>
                      ) : (
                        <p className="text-text-faint text-xs">
                          Lowercase letters, numbers and hyphens. Used to
                          identify the stored volume.
                        </p>
                      )}
                    </Field>
                  );
                }}
              />
              <addForm.Field
                name="mountPath"
                validators={{
                  onChange: z
                    .string()
                    .min(1, "Mount path is required")
                    .refine(
                      (v) => v.startsWith("/"),
                      "Must be an absolute path",
                    )
                    .refine(
                      (v) => !SYSTEM_PATHS.has(v),
                      "Cannot be a system path",
                    ),
                }}
                children={(field) => {
                  const error = fieldError(field.state.meta.errors);
                  return (
                    <Field data-invalid={!!error}>
                      <FieldLabel htmlFor="volume-mount-path">
                        Mount path
                      </FieldLabel>
                      <Input
                        id="volume-mount-path"
                        placeholder="/data"
                        className="font-mono"
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        aria-invalid={!!error}
                      />
                      {error ? (
                        <FieldError>{error}</FieldError>
                      ) : (
                        <p className="text-text-faint text-xs">
                          Absolute path inside the container, e.g.{" "}
                          <code>/data</code>. Cannot be a system path like{" "}
                          <code>/</code>, <code>/tmp</code> or <code>/etc</code>
                          .
                        </p>
                      )}
                    </Field>
                  );
                }}
              />
            </div>
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                onClick={() => setAddOpen(false)}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={createVolume.isPending}>
                {createVolume.isPending ? "Creating..." : "Add Volume"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* Remove volume dialog */}
      <AlertDialog
        open={removeTarget !== null}
        onOpenChange={(open) => {
          if (!open) {
            setRemoveTarget(null);
            setDeleteData(false);
          }
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Remove {removeTarget?.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              {removeTarget?.external_name
                ? `The adopted volume ${removeTarget.external_name} will be detached on the next deploy. Its data is left behind and remains protected from cleanup. It can be adopted again when unattached.`
                : "The volume will be detached on the next deploy. Stored data is kept unless you choose to delete it."}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {!removeTarget?.external_name && (
            <Label className="flex items-start gap-2 rounded-md border p-3 text-sm font-normal">
              <Checkbox
                className="mt-0.5"
                checked={deleteData}
                onCheckedChange={setDeleteData}
              />
              <span>
                <span className="text-destructive font-medium">
                  Also permanently delete the stored data
                </span>
                <span className="text-muted-foreground block">
                  This cannot be undone. The underlying data volume is
                  destroyed.
                </span>
              </span>
            </Label>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={submitRemove}
              disabled={deleteVolume.isPending}
            >
              {deleteData ? "Delete volume & data" : "Remove volume"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}
