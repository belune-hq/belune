import { createFileRoute } from "@tanstack/react-router";
import { ProcessTypesSection } from "@/components/applications/process-types-section";

export const Route = createFileRoute(
  "/_app/projects/$projectId/applications/$applicationId/processes",
)({
  component: ApplicationProcessesPage,
});

function ApplicationProcessesPage() {
  const { projectId, applicationId } = Route.useParams();
  return (
    <ProcessTypesSection projectId={projectId} applicationId={applicationId} />
  );
}
