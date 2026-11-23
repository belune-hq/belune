import { useEffect } from "react";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";
import { GitBranchIcon, Link2, KeyRound, Package } from "lucide-react";
import { RouteError } from "@/lib/components/route-error";
import { useAuthStore } from "@/lib/stores/auth";
import { PageHeader } from "@/components/ui/page-header";
import { PageTabs, type PageTab } from "@/components/ui/page-tabs";
import { ConnectionsPanel } from "@/components/git/connections-panel";
import { RegistriesSection } from "@/components/git/registries-section";
import { ProvidersPanel } from "@/components/git/providers-panel";

type SourcesTab = "connections" | "providers" | "registries";

const SOURCES_TABS: PageTab<SourcesTab>[] = [
  { value: "connections", label: "Connections", icon: Link2 },
  { value: "providers", label: "Providers", icon: KeyRound },
  { value: "registries", label: "Registries", icon: Package },
];

export const Route = createFileRoute("/_app/sources")({
  component: SourcesPage,
  errorComponent: RouteError,
  validateSearch: (search: Record<string, unknown>) => ({
    tab:
      search.tab === "providers" || search.tab === "registries"
        ? search.tab
        : undefined,
    connected:
      typeof search.connected === "string" ? search.connected : undefined,
    error: typeof search.error === "string" ? search.error : undefined,
  }),
});

const PROVIDER_LABELS: Record<string, string> = {
  github: "GitHub",
  gitlab: "GitLab",
  bitbucket: "Bitbucket",
  gitea: "Gitea",
};

function SourcesPage() {
  const { tab, connected, error } = Route.useSearch();
  const navigate = useNavigate({ from: "/sources" });
  const isAdmin = useAuthStore((s) => s.user?.role === "admin");

  // Non-admins never get either admin tab, including a direct URL request.
  const activeTab: SourcesTab =
    isAdmin && (tab === "providers" || tab === "registries")
      ? tab
      : "connections";

  // Surface OAuth-connect / GitHub-App-creation outcomes from the callback redirect.
  useEffect(() => {
    if (!connected && !error) return;
    if (connected === "github" && tab === "providers") {
      toast.success("GitHub App created");
    } else if (connected) {
      toast.success(`Connected ${PROVIDER_LABELS[connected] ?? connected}`);
    } else if (error) {
      toast.error(`Failed to connect ${PROVIDER_LABELS[error] ?? error}`);
    }
    // Clear the one-shot params so the toast doesn't refire on refresh.
    navigate({
      search: (s) => ({ ...s, connected: undefined, error: undefined }),
      replace: true,
    });
  }, [connected, error, tab, navigate]);

  const setTab = (next: SourcesTab) =>
    navigate({
      search: (s) => ({
        ...s,
        tab: next === "connections" ? undefined : next,
      }),
    });

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        icon={<GitBranchIcon className="size-5" />}
        title="Sources"
        description={
          <>
            Connect accounts to deploy repositories
            {isAdmin
              ? ", manage provider apps and install-wide registry credentials."
              : "."}
          </>
        }
      />

      {isAdmin ? (
        <>
          <PageTabs
            ariaLabel="Sources views"
            items={SOURCES_TABS}
            value={activeTab}
            onValueChange={setTab}
          />
          {activeTab === "registries" ? (
            <RegistriesSection />
          ) : activeTab === "providers" ? (
            <ProvidersPanel />
          ) : (
            <ConnectionsPanel />
          )}
        </>
      ) : (
        <ConnectionsPanel />
      )}
    </div>
  );
}
