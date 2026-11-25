import { useForm } from "@tanstack/react-form";
import { toast } from "sonner";
import { z } from "zod";
import { useAdoptVolume } from "@/lib/hooks/use-volumes";
import { fieldError } from "@/lib/utils/field-error";
import { Button } from "@/components/ui/button";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

export function AdoptVolumeDialog({
  projectId,
  applicationId,
  open,
  onOpenChange,
}: {
  projectId: string;
  applicationId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const adopt = useAdoptVolume(projectId, applicationId);
  const form = useForm({
    defaultValues: { name: "", externalName: "", mountPath: "" },
    onSubmit: async ({ value }) => {
      try {
        await adopt.mutateAsync({
          name: value.name,
          external_name: value.externalName,
          mount_path: value.mountPath,
        });
        toast.success("Volume adopted — reload the application to mount it");
        onOpenChange(false);
        form.reset();
      } catch (error) {
        toast.error(
          error instanceof Error ? error.message : "Could not adopt volume",
        );
      }
    },
  });
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) form.reset();
        onOpenChange(next);
      }}
    >
      <DialogContent>
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            e.stopPropagation();
            void form.handleSubmit();
          }}
        >
          <DialogHeader>
            <DialogTitle>Adopt Existing Volume</DialogTitle>
            <DialogDescription>
              Attach a Docker volume that already exists on this server. Its
              data is retained when this application or mount is removed. Remove
              any external container using it before adopting.
            </DialogDescription>
          </DialogHeader>
          <form.Field
            name="externalName"
            validators={{
              onChange: z
                .string()
                .min(1, "Docker volume name is required")
                .max(255)
                .regex(
                  /^[A-Za-z0-9][A-Za-z0-9_.-]*$/,
                  "Enter the exact Docker volume name",
                )
                .refine(
                  (v) => !v.startsWith("belune-"),
                  "Belune-managed volumes cannot be adopted",
                ),
            }}
          >
            {(field) => {
              const error = fieldError(field.state.meta.errors);
              return (
                <Field data-invalid={!!error}>
                  <FieldLabel htmlFor="adopt-external-name">
                    Docker volume name
                  </FieldLabel>
                  <Input
                    id="adopt-external-name"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                    aria-invalid={!!error}
                    placeholder="myapp_pgdata"
                  />
                  {error && <FieldError>{error}</FieldError>}
                </Field>
              );
            }}
          </form.Field>
          <form.Field
            name="name"
            validators={{
              onChange: z
                .string()
                .min(1, "Mount name is required")
                .max(40)
                .regex(
                  /^[a-z0-9-]+$/,
                  "Use lowercase letters, numbers and hyphens",
                ),
            }}
          >
            {(field) => {
              const error = fieldError(field.state.meta.errors);
              return (
                <Field data-invalid={!!error}>
                  <FieldLabel htmlFor="adopt-name">Mount name</FieldLabel>
                  <Input
                    id="adopt-name"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                    aria-invalid={!!error}
                    placeholder="data"
                  />
                  {error && <FieldError>{error}</FieldError>}
                </Field>
              );
            }}
          </form.Field>
          <form.Field
            name="mountPath"
            validators={{
              onChange: z
                .string()
                .min(1, "Mount path is required")
                .startsWith("/", "Use an absolute path inside the container"),
            }}
          >
            {(field) => {
              const error = fieldError(field.state.meta.errors);
              return (
                <Field data-invalid={!!error}>
                  <FieldLabel htmlFor="adopt-mount-path">Mount path</FieldLabel>
                  <Input
                    id="adopt-mount-path"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                    aria-invalid={!!error}
                    placeholder="/var/lib/postgresql/data"
                  />
                  {error && <FieldError>{error}</FieldError>}
                </Field>
              );
            }}
          </form.Field>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => {
                form.reset();
                onOpenChange(false);
              }}
            >
              Cancel
            </Button>
            <form.Subscribe selector={(s) => s.canSubmit}>
              {(canSubmit) => (
                <Button type="submit" disabled={!canSubmit || adopt.isPending}>
                  {adopt.isPending ? "Adopting…" : "Adopt volume"}
                </Button>
              )}
            </form.Subscribe>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
