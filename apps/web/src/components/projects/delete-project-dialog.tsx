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
import { useDeleteProject } from "@/lib/hooks/use-projects";

interface Props {
  projectId: string;
  projectName: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

/**
 * Mirrors DeleteApplicationDialog — deliberately, so the two most destructive
 * actions in the product ask for the same thing in the same way. This one
 * destroys strictly more: every application AND database in the project goes
 * with it, each through its own delete path — so database backups, remote
 * copies included, go too.
 */
export function DeleteProjectDialog({
  projectId,
  projectName,
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
        <DeleteProjectDialogBody
          key={body.key}
          projectId={projectId}
          projectName={projectName}
          onDone={() => onOpenChange(false)}
        />
      </AlertDialogContent>
    </AlertDialog>
  );
}

function DeleteProjectDialogBody({
  projectId,
  projectName,
  onDone,
}: {
  projectId: string;
  projectName: string;
  onDone: () => void;
}) {
  const navigate = useNavigate();
  const deleteProject = useDeleteProject();
  const inputId = useId();
  const [confirmText, setConfirmText] = useState("");

  // Trimmed because a trailing space from copy-paste is not a different
  // project, but otherwise exact: matching case is the deliberate act.
  const confirmed = confirmText.trim() === projectName.trim();

  return (
    <>
      <AlertDialogHeader>
        <AlertDialogTitle>Delete {projectName}?</AlertDialogTitle>
        <AlertDialogDescription>
          This will permanently delete the project and everything in it — every
          application and database, their containers and managed volumes, and
          all configuration including environment variables, domains, backup
          destinations and schedules. This action cannot be undone.
        </AlertDialogDescription>
        <AlertDialogDescription>
          Adopted application volumes are left behind and remain protected from
          cleanup.
        </AlertDialogDescription>
        {/* Backups are called out separately because they are the one thing
            that survives losing the server, so an operator may reasonably
            believe they are a safety net here. Project deletion removes each
            database and application through its own delete path, and both
            erase remote copies. ⚠️ Volume backups are named here only
            because application deletion now actually erases them — before
            that fix their objects were left behind, so claiming destruction
            would have been false in a new direction. Static wording, not a
            count: an accurate number needs impact
            aggregated across databases and volumes (see the project-deletion
            gap), and a partial count on a confirmation dialog would be worse
            than none. Worded so it claims nothing about what the project
            actually contains. */}
        <AlertDialogDescription className="text-destructive font-medium">
          Every backup of its databases and volumes is destroyed as well,
          including copies already uploaded to a remote destination. Restore
          from them will no longer be possible.
        </AlertDialogDescription>
      </AlertDialogHeader>

      <div className="space-y-2">
        <Label htmlFor={inputId} className="font-normal">
          Type{" "}
          <span className="text-foreground font-medium">{projectName}</span> to
          confirm.
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
          disabled={!confirmed}
          onClick={() => {
            toast.promise(
              deleteProject.mutateAsync(projectId).then(() => {
                onDone();
                navigate({ to: "/projects" });
              }),
              {
                loading: "Deleting project...",
                success: "Project deleted",
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
