import { createFileRoute } from "@tanstack/react-router";
import { ScheduledTasksSection } from "@/components/applications/scheduled-tasks-section";

export const Route = createFileRoute(
  "/_app/projects/$projectId/applications/$applicationId/tasks",
)({
  component: ApplicationTasksPage,
});

function ApplicationTasksPage() {
  const { projectId, applicationId } = Route.useParams();
  return (
    <ScheduledTasksSection
      projectId={projectId}
      applicationId={applicationId}
    />
  );
}
