import { ScheduleText } from "@/components/ui/schedule-text";
import { useState } from "react";
import { toast } from "sonner";
import {
  DatabaseBackupIcon,
  HistoryIcon,
  PencilIcon,
  PlusIcon,
  Trash2Icon,
} from "lucide-react";
import type { AppVolumeBackupConfig } from "@/lib/types";
import { useVolumes } from "@/lib/hooks/use-volumes";
import { useBackupDestinations } from "@/lib/hooks/use-backup-destinations";
import {
  useAppVolumeBackupConfigs,
  useRunVolumeBackup,
  useDeleteVolumeBackupConfig,
} from "@/lib/hooks/use-volume-backups";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { IconAction } from "@/components/ui/icon-action";
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
import { VolumeBackupConfigForm } from "./volume-backup-config-form";
import { VolumeBackupConfigDrawer } from "./volume-backup-config-drawer";

interface Props {
  projectId: string;
  applicationId: string;
}

export function VolumeBackupConfigsSection({
  projectId,
  applicationId,
}: Props) {
  const { data: configs, isLoading } = useAppVolumeBackupConfigs(
    projectId,
    applicationId,
  );
  const { data: destinations } = useBackupDestinations(projectId);
  const { data: volumes } = useVolumes(projectId, applicationId);

  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<AppVolumeBackupConfig | null>(null);
  const [manageTarget, setManageTarget] =
    useState<AppVolumeBackupConfig | null>(null);

  const destName = (id: string) =>
    destinations?.find((d) => d.id === id)?.name ?? "Unknown destination";
  const noDestinations = destinations?.length === 0;
  const noVolumes = volumes?.length === 0;

  const openAdd = () => {
    setEditing(null);
    setFormOpen(true);
  };
  const openEdit = (cfg: AppVolumeBackupConfig) => {
    setManageTarget(null);
    setEditing(cfg);
    setFormOpen(true);
  };

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <CardTitle className="flex items-center gap-2">
              <DatabaseBackupIcon aria-hidden="true" className="size-4" />
              Backups
            </CardTitle>
            <CardDescription className="mt-1">
              Snapshot a volume to a project destination, on a schedule or on
              demand. A volume can back up to multiple destinations.
            </CardDescription>
          </div>
          <Button
            size="sm"
            className="shrink-0"
            onClick={openAdd}
            disabled={noDestinations || noVolumes}
          >
            <PlusIcon aria-hidden="true" className="size-4" />
            Add Backup
          </Button>
        </div>
      </CardHeader>
      <CardContent>
        {noVolumes ? (
          <p className="text-muted-foreground text-sm">
            Add a volume first, then configure backups for it.
          </p>
        ) : noDestinations ? (
          <p className="text-muted-foreground text-sm">
            Add a destination on the project{" "}
            <span className="font-medium">Backups</span> tab before configuring
            backups.
          </p>
        ) : isLoading ? (
          <Skeleton className="h-16 w-full" />
        ) : !configs || configs.length === 0 ? (
          <p className="text-muted-foreground py-4 text-center text-sm">
            No backups configured yet.
          </p>
        ) : (
          <ul className="divide-border divide-y">
            {configs.map((c) => (
              <ConfigRow
                key={c.id}
                projectId={projectId}
                applicationId={applicationId}
                config={c}
                destinationName={destName(c.destination_id)}
                onEdit={openEdit}
                onManage={setManageTarget}
              />
            ))}
          </ul>
        )}
      </CardContent>

      <VolumeBackupConfigForm
        projectId={projectId}
        applicationId={applicationId}
        config={editing}
        open={formOpen}
        onOpenChange={setFormOpen}
      />
      {manageTarget && (
        <VolumeBackupConfigDrawer
          projectId={projectId}
          applicationId={applicationId}
          config={manageTarget}
          destinationName={destName(manageTarget.destination_id)}
          open={manageTarget !== null}
          onOpenChange={(o) => !o && setManageTarget(null)}
        />
      )}
    </Card>
  );
}

function ConfigRow({
  projectId,
  applicationId,
  config,
  destinationName,
  onEdit,
  onManage,
}: {
  projectId: string;
  applicationId: string;
  config: AppVolumeBackupConfig;
  destinationName: string;
  onEdit: (config: AppVolumeBackupConfig) => void;
  onManage: (config: AppVolumeBackupConfig) => void;
}) {
  const runBackup = useRunVolumeBackup(
    projectId,
    applicationId,
    config.application_volume_id,
  );
  const deleteConfig = useDeleteVolumeBackupConfig(
    projectId,
    applicationId,
    config.application_volume_id,
  );
  const [confirmOpen, setConfirmOpen] = useState(false);

  const backUpNow = () => {
    toast.promise(runBackup.mutateAsync(config.id), {
      loading: "Starting backup...",
      success: "Backup started",
      error: (err) => err.message,
    });
  };

  const remove = () => {
    toast.promise(deleteConfig.mutateAsync(config.id), {
      loading: "Removing config...",
      success: "Backup config removed",
      error: (err) => err.message,
    });
  };

  return (
    <li className="flex items-center justify-between gap-3 py-3">
      <div className="min-w-0 space-y-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-medium">{config.volume_name}</span>
          <span className="text-text-faint font-mono text-xs">
            {config.mount_path}
          </span>
          {config.enabled ? (
            <Badge variant="outline">Active</Badge>
          ) : (
            <Badge variant="secondary">Disabled</Badge>
          )}
        </div>
        <p className="text-text-faint text-xs">
          {destinationName}
          {" · "}
          {config.schedule ? (
            <ScheduleText cron={config.schedule} timezone={config.timezone} />
          ) : (
            "manual only"
          )}
          {config.prefix ? ` · ${config.prefix}` : ""}
          {config.keep_latest != null ? ` · keep ${config.keep_latest}` : ""}
        </p>
      </div>
      <div className="flex shrink-0 items-center gap-1">
        <IconAction
          label="Back up now"
          size="icon-sm"
          disabled={runBackup.isPending}
          onClick={backUpNow}
        >
          <DatabaseBackupIcon aria-hidden="true" className="size-4" />
        </IconAction>
        <IconAction
          label="Manage Histories"
          size="icon-sm"
          onClick={() => onManage(config)}
        >
          <HistoryIcon aria-hidden="true" className="size-4" />
        </IconAction>
        <IconAction label="Edit" size="icon-sm" onClick={() => onEdit(config)}>
          <PencilIcon aria-hidden="true" className="size-4" />
        </IconAction>
        <IconAction
          label="Delete"
          size="icon-sm"
          destructive
          onClick={() => setConfirmOpen(true)}
        >
          <Trash2Icon aria-hidden="true" className="size-4" />
        </IconAction>
      </div>

      <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete backup config?</AlertDialogTitle>
            <AlertDialogDescription>
              This removes the schedule for{" "}
              <span className="font-medium">{config.volume_name}</span> and
              deletes the backups it produced from{" "}
              <span className="font-medium">{destinationName}</span>. This can't
              be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={remove}
              disabled={deleteConfig.isPending}
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </li>
  );
}
