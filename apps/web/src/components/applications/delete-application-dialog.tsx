import { useVolumes } from "@/lib/hooks/use-volumes";
import { useDialogBody } from "@/lib/hooks/use-dialog-body";
import { useId, useState } from "react";
import { useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";
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
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useDeleteApplication } from "@/lib/hooks/use-applications";

interface Props {
  projectId: string;
  applicationId: string;
  applicationName: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function DeleteApplicationDialog({
  projectId,
  applicationId,
  applicationName,
  open,
  onOpenChange,
}: Props) {
  // Remount on every open so confirmText always starts blank — a user who
  // typed the name, cancelled, then reopened would otherwise find the
  // confirmation already satisfied. See useDialogBody.
  const body = useDialogBody(open, null);

  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <DeleteApplicationDialogBody
          key={body.key}
          projectId={projectId}
          applicationId={applicationId}
          applicationName={applicationName}
          onDone={() => onOpenChange(false)}
        />
      </AlertDialogContent>
    </AlertDialog>
  );
}

function DeleteApplicationDialogBody({
  projectId,
  applicationId,
  applicationName,
  onDone,
}: {
  projectId: string;
  applicationId: string;
  applicationName: string;
  onDone: () => void;
}) {
  const navigate = useNavigate();
  const deleteApplication = useDeleteApplication(projectId);
  const {
    data: volumes,
    isPending: volumesPending,
    isError: volumesFailed,
  } = useVolumes(projectId, applicationId);
  const managed = volumes?.filter((v) => !v.external_name) ?? [];
  const adopted = volumes?.filter((v) => v.external_name) ?? [];
  const inputId = useId();
  const [confirmText, setConfirmText] = useState("");

  // Trimmed because a trailing space from copy-paste is not a different
  // application, but otherwise exact: matching case is the deliberate act.
  const confirmed = confirmText.trim() === applicationName.trim();

  return (
    <>
      <AlertDialogHeader>
        <AlertDialogTitle>Delete {applicationName}?</AlertDialogTitle>
        <AlertDialogDescription>
          This will stop the running container and permanently delete this
          application, its managed data volumes, file mounts, domains,
          environment variables and deployment history. This action cannot be
          undone.
        </AlertDialogDescription>
        <AlertDialogDescription>
          {volumesPending ? (
            "Checking volumes…"
          ) : volumesFailed ? (
            "Could not check volumes. Try again before deleting."
          ) : (
            <>
              {managed.length} managed volume{managed.length === 1 ? "" : "s"}{" "}
              will be removed
              {managed.length > 0
                ? `: ${managed.map((v) => v.name).join(", ")}`
                : ""}
              . {adopted.length} adopted volume{adopted.length === 1 ? "" : "s"}{" "}
              will be left behind and remain protected from cleanup
              {adopted.length > 0
                ? `: ${adopted.map((v) => v.external_name).join(", ")}`
                : ""}
              .
            </>
          )}
        </AlertDialogDescription>
        <AlertDialogDescription className="text-destructive font-medium">
          Every backup of its volumes is destroyed as well, including copies
          already uploaded to a remote destination. Restore from them will no
          longer be possible.
        </AlertDialogDescription>
      </AlertDialogHeader>

      <div className="space-y-2">
        <Label htmlFor={inputId} className="font-normal">
          Type{" "}
          <span className="text-foreground font-medium">{applicationName}</span>{" "}
          to confirm.
        </Label>
        <Input
          id={inputId}
          value={confirmText}
          onChange={(e) => setConfirmText(e.target.value)}
          autoComplete="off"
          autoCorrect="off"
          spellCheck={false}
        />
      </div>

      <AlertDialogFooter>
        <AlertDialogCancel>Cancel</AlertDialogCancel>
        <AlertDialogAction
          variant="destructive-solid"
          disabled={!confirmed || volumesPending || volumesFailed}
          onClick={() => {
            toast.promise(
              deleteApplication.mutateAsync(applicationId).then(() => {
                onDone();
                navigate({
                  to: "/projects/$projectId",
                  params: { projectId },
                });
              }),
              {
                loading: "Deleting application...",
                success: "Application deleted",
                error: (err) => err.message,
              },
            );
          }}
        >
          Delete
        </AlertDialogAction>
      </AlertDialogFooter>
    </>
  );
}
