import {
  TimezoneInput,
  browserTimezone,
  isKnownTimezone,
} from "@/components/ui/timezone-input";
import { useDialogBody } from "@/lib/hooks/use-dialog-body";
import { useState } from "react";
import { useForm, useStore } from "@tanstack/react-form";
import { toast } from "sonner";
import { z } from "zod";
import { ClockIcon, CloudIcon, HardDriveIcon } from "lucide-react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { cronError } from "@/lib/utils/schedule";
import { fieldError } from "@/lib/utils/field-error";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useVolumes } from "@/lib/hooks/use-volumes";
import { useBackupDestinations } from "@/lib/hooks/use-backup-destinations";
import {
  useCreateVolumeBackupConfig,
  useUpdateVolumeBackupConfig,
} from "@/lib/hooks/use-volume-backups";
import type { AppVolumeBackupConfig } from "@/lib/types";

interface Props {
  projectId: string;
  applicationId: string;
  config?: AppVolumeBackupConfig | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

const SCHEDULE_PRESETS: { value: string; label: string }[] = [
  { value: "0 * * * *", label: "Every hour" },
  { value: "0 0 * * *", label: "Every day at midnight" },
  { value: "0 0 * * 0", label: "Every week (Sunday midnight)" },
  { value: "0 0 1 * *", label: "Every month (1st, midnight)" },
];

export function VolumeBackupConfigForm({
  projectId,
  applicationId,
  config,
  open,
  onOpenChange,
}: Props) {
  // Fields initialise from props on each open; see useDialogBody.
  const body = useDialogBody(open, config);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <ConfigForm
          key={body.key}
          projectId={projectId}
          applicationId={applicationId}
          config={body.target}
          onDone={() => onOpenChange(false)}
        />
      </DialogContent>
    </Dialog>
  );
}

function ConfigForm({
  projectId,
  applicationId,
  config,
  onDone,
}: {
  projectId: string;
  applicationId: string;
  config?: AppVolumeBackupConfig | null;
  onDone: () => void;
}) {
  const editing = !!config;
  const { data: volumes } = useVolumes(projectId, applicationId);
  const { data: destinations } = useBackupDestinations(projectId);

  const [customMode, setCustomMode] = useState(
    (config?.schedule ?? "") !== "" &&
      !SCHEDULE_PRESETS.some((p) => p.value === config?.schedule),
  );

  const form = useForm({
    defaultValues: {
      volumeId: config?.application_volume_id ?? "",
      destinationId: config?.destination_id ?? "",
      prefix: config?.prefix ?? "",
      schedule: config?.schedule ?? "",
      timezone: config?.timezone ?? browserTimezone(),
      keepLatest: config?.keep_latest != null ? String(config.keep_latest) : "",
      quiesce: config?.quiesce ?? false,
      enabled: config?.enabled ?? true,
    },
    onSubmit: ({ value }) => {
      const keep =
        value.keepLatest.trim() === "" ? null : Number(value.keepLatest);
      const data = {
        destination_id: value.destinationId,
        prefix: value.prefix.trim(),
        schedule: value.schedule.trim(),
        timezone: value.timezone.trim(),
        keep_latest: keep,
        quiesce: value.quiesce,
        enabled: value.enabled,
      };
      const action =
        editing && config ? update.mutateAsync(data) : create.mutateAsync(data);
      toast.promise(action, {
        loading: editing ? "Saving backup…" : "Creating backup…",
        success: () => {
          onDone();
          return editing ? "Backup saved" : "Backup created";
        },
        error: (err) => err.message,
      });
    },
  });

  const volumeId = useStore(form.store, (s) => s.values.volumeId);
  const create = useCreateVolumeBackupConfig(
    projectId,
    applicationId,
    volumeId,
  );
  const update = useUpdateVolumeBackupConfig(
    projectId,
    applicationId,
    volumeId,
    config?.id ?? "",
  );

  const pending = create.isPending || update.isPending;
  const attempted = useStore(form.store, (s) => s.submissionAttempts > 0);
  const noDestinations = destinations?.length === 0;
  const noVolumes = volumes?.length === 0;

  return (
    <>
      <DialogHeader>
        <DialogTitle>{editing ? "Edit Backup" : "Add Backup"}</DialogTitle>
        <DialogDescription>
          Back up a volume to a project destination, on a schedule or on demand.
        </DialogDescription>
      </DialogHeader>

      <form
        onSubmit={(e) => {
          e.preventDefault();
          e.stopPropagation();
          form.handleSubmit();
        }}
        className="space-y-4"
      >
        <form.Field
          name="volumeId"
          validators={{
            onChange: z.string().min(1, "Select a volume"),
          }}
          children={(field) => {
            const error = fieldError(field.state.meta.errors);
            return (
              <Field data-invalid={!!error && !editing}>
                <FieldLabel>Volume</FieldLabel>
                {editing ? (
                  <div className="border-input flex items-center gap-2 rounded-md border px-3 py-2 text-sm">
                    <HardDriveIcon aria-hidden="true" className="size-4" />
                    <span className="font-medium">{config?.volume_name}</span>
                    <span className="text-text-faint font-mono text-xs">
                      {config?.mount_path}
                    </span>
                  </div>
                ) : (
                  <Select
                    value={field.state.value}
                    onValueChange={(v) => field.handleChange(v ?? "")}
                  >
                    <SelectTrigger aria-invalid={!!error}>
                      <SelectValue placeholder="Select a volume" />
                    </SelectTrigger>
                    <SelectContent>
                      {(volumes ?? []).map((v) => (
                        <SelectItem
                          key={v.id}
                          value={v.id}
                          icon={<HardDriveIcon />}
                        >
                          {v.name}
                          <span className="text-text-faint ml-1 font-mono text-xs">
                            {v.mount_path}
                          </span>
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
                {error && !editing ? (
                  <FieldError>{error}</FieldError>
                ) : (
                  noVolumes && (
                    <p className="text-muted-foreground text-xs">
                      No volumes yet — add one in the Volumes list first.
                    </p>
                  )
                )}
              </Field>
            );
          }}
        />

        <form.Field
          name="destinationId"
          validators={{
            onChange: z.string().min(1, "Select a destination"),
          }}
          children={(field) => {
            const error = fieldError(field.state.meta.errors);
            return (
              <Field data-invalid={!!error}>
                <FieldLabel>Destination</FieldLabel>
                <Select
                  value={field.state.value}
                  onValueChange={(v) => field.handleChange(v ?? "")}
                >
                  <SelectTrigger aria-invalid={!!error}>
                    <SelectValue placeholder="Select destination" />
                  </SelectTrigger>
                  <SelectContent>
                    {(destinations ?? []).map((d) => (
                      <SelectItem key={d.id} value={d.id} icon={<CloudIcon />}>
                        {d.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {error ? (
                  <FieldError>{error}</FieldError>
                ) : (
                  noDestinations && (
                    <p className="text-muted-foreground text-xs">
                      No destinations yet — add one on the project Backups tab
                      first.
                    </p>
                  )
                )}
              </Field>
            );
          }}
        />

        <form.Field
          name="prefix"
          children={(field) => (
            <div className="space-y-1.5">
              <Label htmlFor="vbc-prefix">Prefix</Label>
              <Input
                id="vbc-prefix"
                value={field.state.value}
                onBlur={field.handleBlur}
                onChange={(e) => field.handleChange(e.target.value)}
                placeholder="/my-app"
                className="font-mono"
              />
              <p className="text-muted-foreground text-xs">
                Optional path inside the bucket to store backups under.
              </p>
            </div>
          )}
        />

        <form.Field
          name="schedule"
          validators={{
            onChange: z.string().superRefine((v, ctx) => {
              const msg = cronError(v);
              if (msg) ctx.addIssue({ code: "custom", message: msg });
            }),
          }}
          children={(field) => {
            // Gated on isBlurred (or a submit attempt), not isTouched: TanStack sets
            // isTouched on every change, so it would flash red while a valid
            // expression is still being typed (the v0.1.21 failure mode).
            const error =
              field.state.meta.isBlurred || attempted
                ? fieldError(field.state.meta.errors)
                : undefined;
            const presetValue = customMode
              ? "custom"
              : field.state.value === ""
                ? "manual"
                : (SCHEDULE_PRESETS.find((p) => p.value === field.state.value)
                    ?.value ?? "custom");
            return (
              <Field data-invalid={!!error}>
                <FieldLabel>Schedule</FieldLabel>
                <Select
                  value={presetValue}
                  onValueChange={(v) => {
                    if (v === "manual") {
                      setCustomMode(false);
                      field.handleChange("");
                    } else if (v === "custom") {
                      setCustomMode(true);
                    } else if (v) {
                      setCustomMode(false);
                      field.handleChange(v);
                    }
                  }}
                >
                  <SelectTrigger>
                    <SelectValue placeholder="Select a schedule" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="manual" icon={<ClockIcon />}>
                      Manual only (no schedule)
                    </SelectItem>
                    {SCHEDULE_PRESETS.map((p) => (
                      <SelectItem
                        key={p.value}
                        value={p.value}
                        icon={<ClockIcon />}
                      >
                        {p.label}
                      </SelectItem>
                    ))}
                    <SelectItem value="custom" icon={<ClockIcon />}>
                      Custom…
                    </SelectItem>
                  </SelectContent>
                </Select>
                <Input
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(e) => {
                    setCustomMode(false);
                    field.handleChange(e.target.value);
                  }}
                  placeholder="0 0 * * * (leave empty for manual only)"
                  className="font-mono"
                  aria-label="Cron expression"
                  aria-invalid={!!error}
                />
                {error ? (
                  <FieldError>{error}</FieldError>
                ) : (
                  <p className="text-muted-foreground text-xs">
                    Standard 5-field cron expression. Empty = back up on demand
                    only.
                  </p>
                )}
              </Field>
            );
          }}
        />

        <form.Field
          name="timezone"
          validators={{
            onChange: z
              .string()
              .trim()
              .refine(isKnownTimezone, "Unknown timezone"),
          }}
          children={(field) => {
            const error = fieldError(field.state.meta.errors);
            return (
              <Field data-invalid={!!error}>
                <FieldLabel htmlFor="vbc-tz">Timezone</FieldLabel>
                <TimezoneInput
                  id="vbc-tz"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(e) => field.handleChange(e.target.value)}
                  aria-invalid={!!error}
                />
                {error ? (
                  <FieldError>{error}</FieldError>
                ) : (
                  <p className="text-muted-foreground text-xs">
                    The schedule above is read in this timezone.
                  </p>
                )}
              </Field>
            );
          }}
        />

        <form.Field
          name="keepLatest"
          validators={{
            onChange: z
              .string()
              .refine(
                (v) => v.trim() === "" || /^\d+$/.test(v.trim()),
                "Must be a whole number",
              )
              .refine(
                (v) => v.trim() === "" || Number(v) >= 1,
                "Must be at least 1",
              ),
          }}
          children={(field) => {
            const error = fieldError(field.state.meta.errors);
            return (
              <Field data-invalid={!!error}>
                <FieldLabel htmlFor="vbc-keep">Keep latest</FieldLabel>
                <Input
                  id="vbc-keep"
                  type="number"
                  min={1}
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(e) => field.handleChange(e.target.value)}
                  placeholder="Keeps all if empty"
                  aria-invalid={!!error}
                />
                {error ? (
                  <FieldError>{error}</FieldError>
                ) : (
                  <p className="text-muted-foreground text-xs">
                    Optional. Only keep the latest N backups in the destination.
                  </p>
                )}
              </Field>
            );
          }}
        />

        <form.Field
          name="quiesce"
          children={(field) => (
            <Label className="flex items-start gap-2 text-sm font-normal">
              <Checkbox
                className="mt-0.5"
                checked={field.state.value}
                onCheckedChange={(v) => field.handleChange(v === true)}
              />
              <span>
                Stop the app during backup (quiesce)
                <span className="text-muted-foreground block">
                  Off = live snapshot with no downtime. Turn on for
                  consistency-critical data (e.g. an embedded database).
                </span>
              </span>
            </Label>
          )}
        />

        <form.Field
          name="enabled"
          children={(field) => (
            <Label className="flex items-center gap-2 text-sm font-normal">
              <Checkbox
                checked={field.state.value}
                onCheckedChange={(v) => field.handleChange(v === true)}
              />
              Enabled (run on schedule)
            </Label>
          )}
        />

        <DialogFooter>
          <Button type="submit" disabled={pending}>
            {pending ? "Saving…" : editing ? "Save" : "Create"}
          </Button>
        </DialogFooter>
      </form>
    </>
  );
}
