import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { TriangleAlert, Trash2 } from "lucide-react";
import { useApplication } from "@/lib/hooks/use-applications";
import { useProject } from "@/lib/hooks/use-projects";
import { useAuthStore } from "@/lib/stores/auth";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Separator } from "@/components/ui/separator";
import { ApplicationSettingsForm } from "@/components/applications/application-settings-form";
import { BuildCacheSection } from "@/components/applications/build-cache-section";
import { RuntimeSection } from "@/components/applications/runtime-section";
import { ResourcesSection } from "@/components/applications/resources-section";
import { HealthCheckSection } from "@/components/applications/health-check-section";
import { DeleteApplicationDialog } from "@/components/applications/delete-application-dialog";

export const Route = createFileRoute(
  "/_app/projects/$projectId/applications/$applicationId/settings",
)({
  component: ApplicationSettingsPage,
});

function ApplicationSettingsPage() {
  const { projectId, applicationId } = Route.useParams();
  const { data: application } = useApplication(projectId, applicationId);
  const { data: project } = useProject(projectId);
  const currentUser = useAuthStore((s) => s.user);
  const canDelete =
    currentUser?.role === "admin" || currentUser?.id === project?.user_id;
  const [deleteOpen, setDeleteOpen] = useState(false);

  if (!application) {
    return (
      <div className="space-y-6">
        <Card>
          <CardHeader>
            <Skeleton className="h-5 w-48" />
          </CardHeader>
          <CardContent className="space-y-4">
            {[1, 2, 3].map((i) => (
              <div key={i} className="space-y-2">
                <Skeleton className="h-4 w-24" />
                <Skeleton className="h-9 w-full" />
              </div>
            ))}
          </CardContent>
        </Card>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <ApplicationSettingsForm
        projectId={projectId}
        applicationId={applicationId}
        application={application}
      />

      <Separator />

      <ResourcesSection
        projectId={projectId}
        applicationId={applicationId}
        application={application}
      />

      <Separator />

      <HealthCheckSection
        projectId={projectId}
        applicationId={applicationId}
        application={application}
      />

      {application.type === "git" && (
        <>
          <Separator />
          <BuildCacheSection
            projectId={projectId}
            applicationId={applicationId}
          />
        </>
      )}

      <Separator />

      <RuntimeSection
        projectId={projectId}
        applicationId={applicationId}
        application={application}
      />

      {canDelete && (
        <>
          <Separator />

          {/* ring-, not border-: Card draws its edge with `ring-1` (a box-shadow),
              and Tailwind's preflight zeroes border-width — so the previous
              `border-destructive/50` set a colour on a 0px border and never
              rendered anything. Overriding the ring colour is what actually tints
              the edge. bg/ring use the status-error soft/line tokens so both
              themes get a red that was chosen for them. */}
          <Card className="bg-status-error-soft ring-status-error-line">
            <CardHeader>
              <CardTitle className="text-destructive flex items-center gap-2">
                <TriangleAlert aria-hidden="true" className="size-4" />
                Danger Zone
              </CardTitle>
            </CardHeader>
            <CardContent>
              {/* Row layout mirrors the Server page's Maintenance section: what the
                  action is on the left, the control on the right. space-y-6 +
                  Separator is the shape that section uses, so a second dangerous
                  action drops in without a rewrite. */}
              <div className="space-y-6">
                <div className="flex flex-wrap items-center justify-between gap-3">
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      <Trash2
                        aria-hidden="true"
                        className="text-destructive size-4"
                      />
                      <p className="text-sm font-medium">Delete Application</p>
                    </div>
                    <p className="text-muted-foreground mt-1 text-xs">
                      Stops the running container and permanently deletes this
                      application. This cannot be undone.
                    </p>
                  </div>
                  <Button
                    size="sm"
                    variant="destructive-solid"
                    onClick={() => setDeleteOpen(true)}
                    // "Delete" alone is ambiguous out of context.
                    aria-label="Delete application"
                  >
                    Delete
                  </Button>
                </div>
              </div>
            </CardContent>
          </Card>

          <DeleteApplicationDialog
            projectId={projectId}
            applicationId={applicationId}
            applicationName={application.name}
            open={deleteOpen}
            onOpenChange={setDeleteOpen}
          />
        </>
      )}
    </div>
  );
}
