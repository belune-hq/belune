import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { RouteIcon, ShieldIcon, ShieldCheckIcon } from "lucide-react";
import { PageHeader } from "@/components/ui/page-header";
import { PageTabs, type PageTab } from "@/components/ui/page-tabs";
import { CaddyTab } from "@/components/proxy/caddy-tab";
import { ModuleTab } from "@/components/proxy/module-tab";
import { RouteError } from "@/lib/components/route-error";
import { requireAdmin } from "@/lib/utils/auth-guard";

type ProxyTab = "caddy" | "crowdsec" | "waf";

const PROXY_TABS: PageTab<ProxyTab>[] = [
  { value: "caddy", label: "Caddy", icon: RouteIcon },
  { value: "crowdsec", label: "CrowdSec", icon: ShieldCheckIcon },
  { value: "waf", label: "WAF", icon: ShieldIcon },
];

export const Route = createFileRoute("/_app/proxy")({
  beforeLoad: requireAdmin,
  component: ProxyPage,
  errorComponent: RouteError,
  validateSearch: (search: Record<string, unknown>) => ({
    tab:
      search.tab === "crowdsec" || search.tab === "waf"
        ? (search.tab as ProxyTab)
        : undefined,
  }),
});

function ProxyPage() {
  const { tab } = Route.useSearch();
  const navigate = useNavigate({ from: "/proxy" });
  const activeTab: ProxyTab = tab ?? "caddy";
  const setTab = (next: ProxyTab) =>
    navigate({ search: () => ({ tab: next === "caddy" ? undefined : next }) });

  return (
    <div className="space-y-6">
      <PageHeader
        icon={<RouteIcon className="size-5" />}
        title="Proxy"
        description="The reverse proxy in front of every app: its health, traffic and modules."
      />

      <PageTabs
        ariaLabel="Proxy views"
        items={PROXY_TABS}
        value={activeTab}
        onValueChange={setTab}
      />

      {activeTab === "caddy" ? <CaddyTab /> : <ModuleTab name={activeTab} />}
    </div>
  );
}
