import { useDialogBody } from "@/lib/hooks/use-dialog-body";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { requireAdmin } from "@/lib/utils/auth-guard";
import {
  Activity,
  Archive,
  CalendarIcon,
  ClockIcon,
  ContainerIcon,
  CpuIcon,
  DownloadIcon,
  HardDriveIcon,
  LayoutDashboard,
  MailIcon,
  MemoryStickIcon,
  PowerIcon,
  ServerIcon,
  Settings,
  WrenchIcon,
} from "lucide-react";
import {
  HOST_METRICS_FIELD,
  HOST_METRICS_PRESETS,
  RETENTION_FIELDS,
  RETENTION_PRESETS,
  type RetentionField,
  type RetentionPreset,
} from "@/components/server/retention";
import { RouteError } from "@/lib/components/route-error";
import { UpdateSection } from "@/components/server/update-section";
import { PageTabs, type PageTab } from "@/components/ui/page-tabs";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Badge } from "@/components/ui/badge";
import { LiveIndicator } from "@/components/ui/live-indicator";
import { Button } from "@/components/ui/button";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { fieldError } from "@/lib/utils/field-error";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { StatusPill } from "@/components/ui/status-pill";
import { Sparkline } from "@/components/ui/sparkline";
import { MetricCard } from "@/lib/components/stats/stat-card";
import { PageHeader } from "@/components/ui/page-header";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import {
  Select,
  SelectTrigger,
  SelectContent,
  SelectItem,
} from "@/components/ui/select";
import {
  useMetrics,
  useHostHistoricalMetrics,
  useHostMetricsStream,
  useHostMetricsRange,
  useServerServices,
} from "@/lib/hooks/use-metrics";
import { useSettings, useUpdateSettings } from "@/lib/hooks/use-settings";
import { toast } from "sonner";
import { useForm } from "@tanstack/react-form";
import { z } from "zod";
import { useMemo, useState, type ReactNode } from "react";
import type { HostMetricPoint, SettingEntry } from "@/lib/types";
import { UPlotAreaChart } from "@/components/ui/uplot-area-chart";
import { SystemBackupsPanel } from "@/components/backups/system-backups-panel";
import { MaintenanceSection } from "@/components/server/maintenance-section";
import { useServerIP } from "@/lib/hooks/use-maintenance";
import { DashboardDomainSection } from "@/components/server/dashboard-domain-section";
import { SmtpSection } from "@/components/server/smtp-section";
import { cn } from "@/lib/utils";
import { formatDateTimeShort } from "@/lib/utils/format";
import { useAuthStore } from "@/lib/stores/auth";

type ServerTab = "metric" | "configuration" | "backups";
type HostMetricsView = "overview" | "detail";

const SERVER_TABS: PageTab<ServerTab>[] = [
  { value: "metric", label: "Overview", icon: LayoutDashboard },
  { value: "backups", label: "Backups", icon: Archive },
  { value: "configuration", label: "Configuration", icon: Settings },
];
type CustomRange = { from: string; to: string };

export const Route = createFileRoute("/_app/server")({
  beforeLoad: requireAdmin,
  component: ServerSettingsPage,
  errorComponent: RouteError,
  validateSearch: (search: Record<string, unknown>) => ({
    tab:
      search.tab === "configuration" || search.tab === "backups"
        ? (search.tab as ServerTab)
        : undefined,
  }),
});

function InstanceNameField({
  currentValue,
  updateSettings,
}: {
  currentValue: string;
  updateSettings: ReturnType<typeof useUpdateSettings>;
}) {
  const form = useForm({
    defaultValues: { value: currentValue },
    onSubmit: ({ value }) => {
      toast.promise(
        updateSettings.mutateAsync([
          { key: "instance_name", value: value.value.trim() },
        ]),
        {
          loading: "Saving...",
          success: "Instance name saved",
          error: "Failed to save instance name",
        },
      );
    },
  });

  return (
    <form.Field
      name="value"
      children={(field) => (
        <>
          <Label htmlFor="instance-name">Instance name</Label>
          <div className="flex max-w-md items-center gap-2">
            <Input
              id="instance-name"
              value={field.state.value}
              onChange={(e) => field.handleChange(e.target.value)}
              placeholder="Belune"
            />
            <Button
              onClick={() => form.handleSubmit()}
              disabled={
                updateSettings.isPending ||
                field.state.value.trim() === currentValue.trim()
              }
            >
              Save
            </Button>
          </div>
          <p className="text-muted-foreground text-xs">
            Shown in the sidebar and used as the default GitHub App name when
            connecting a provider.
          </p>
        </>
      )}
    />
  );
}

const IP_REGEX = /^(\d{1,3}\.){3}\d{1,3}$|^[0-9a-fA-F:]+$/;

function ServerIpField({
  currentValue,
  placeholder,
  updateSettings,
}: {
  currentValue: string;
  placeholder: string;
  updateSettings: ReturnType<typeof useUpdateSettings>;
}) {
  const form = useForm({
    defaultValues: { value: currentValue },
    onSubmit: ({ value }) => {
      const trimmed = value.value.trim();
      toast.promise(
        updateSettings.mutateAsync([{ key: "public_ip", value: trimmed }]),
        {
          loading: "Saving...",
          success: trimmed
            ? "Server IP saved"
            : "Server IP cleared (auto-detect)",
          error: (err) => (err as Error).message,
        },
      );
    },
  });

  return (
    <form.Field
      name="value"
      validators={{
        onChange: z
          .string()
          .refine(
            (v) => v.trim() === "" || IP_REGEX.test(v.trim()),
            "Enter a valid IP address",
          ),
      }}
      children={(field) => {
        const error = fieldError(field.state.meta.errors);
        return (
          <Field data-invalid={!!error}>
            <FieldLabel htmlFor="server-ip" className="pt-2">
              Server IP
            </FieldLabel>
            <div className="flex max-w-md items-center gap-2">
              <Input
                id="server-ip"
                value={field.state.value}
                onChange={(e) => field.handleChange(e.target.value)}
                placeholder={placeholder}
                aria-invalid={!!error}
              />
              <Button
                onClick={() => form.handleSubmit()}
                disabled={
                  updateSettings.isPending ||
                  field.state.value.trim() === currentValue.trim()
                }
              >
                Save
              </Button>
            </div>
            {error ? (
              <FieldError>{error}</FieldError>
            ) : (
              <p className="text-muted-foreground text-xs">
                The public address domains must point at for a certificate.
                Leave blank to auto-detect; set it explicitly when the box is
                behind NAT.
              </p>
            )}
          </Field>
        );
      }}
    />
  );
}

function formatTime(iso: string, range: string) {
  const d = new Date(iso);
  if (range === "1h" || range === "3h" || range === "24h")
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  return d.toLocaleDateString([], {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function formatBytes(bytes: number | null) {
  if (bytes == null) return "N/A";
  const gb = bytes / (1024 * 1024 * 1024);
  if (gb >= 1) return `${gb.toFixed(1)} GB`;
  const mb = bytes / (1024 * 1024);
  return `${mb.toFixed(0)} MB`;
}

function pct(part: number | null, total: number | null) {
  if (!part || !total) return 0;
  return Math.round((part / total) * 100);
}

/** nominal < 75% · elevated 75–90% · critical ≥ 90%. */
function loadStatus(percent: number) {
  if (percent >= 90)
    return { label: "critical", className: "text-status-error" };
  if (percent >= 75)
    return { label: "elevated", className: "text-status-building" };
  return { label: "nominal", className: "text-status-ready" };
}

const CPU_COLOR = "hsl(221, 83%, 53%)";
const MEM_COLOR = "hsl(262, 83%, 58%)";
const DISK_COLOR = "hsl(142, 71%, 45%)";
// Swap is deliberately a warm, distinct hue from RAM's violet — the two are read
// as separate resources, never summed.
const SWAP_COLOR = "hsl(28, 90%, 55%)";

// Live host-metrics window: the chart shows the most recent 10 minutes.
const TEN_MIN_MS = 10 * 60 * 1000;

function ServerSettingsPage() {
  const { tab } = Route.useSearch();
  const navigate = useNavigate({ from: "/server" });
  const activeTab: ServerTab =
    tab === "configuration" || tab === "backups" ? tab : "metric";
  const setTab = (next: ServerTab) =>
    navigate({
      search: () => ({ tab: next === "metric" ? undefined : next }),
    });

  const { data: metrics, isLoading } = useMetrics();
  const { data: services } = useServerServices();

  // Host Metrics viewer state: Overview (sparklines) vs Detail (charts), and the
  // time window — null = Live (last 10 min, streamed), otherwise a static snapshot.
  const [hostView, setHostView] = useState<HostMetricsView>("overview");
  const [customRange, setCustomRange] = useState<CustomRange | null>(null);

  // Overview is always live; only Detail honors the custom time range, so the
  // live sources stay enabled regardless of the selected window.
  const { data: historicalData } = useHostHistoricalMetrics("1h");
  // Admin-only: the server refuses metrics:host to anyone else. The route guard
  // already keeps a Member off this page; this keeps the subscription honest on
  // its own instead of relying on that.
  const isAdmin = useAuthStore((s) => s.user?.role === "admin");
  const { data: streamData, connected: streamConnected } =
    useHostMetricsStream(isAdmin);
  const { data: rangeData } = useHostMetricsRange(
    customRange?.from,
    customRange?.to,
  );

  // Live 10-minute window: Overview sparklines, and Detail when unfiltered.
  const liveMetrics = useMemo(() => {
    const ONE_SECOND = 1_000;
    // eslint-disable-next-line react-hooks/purity
    const now = Date.now();
    const maxStart = Math.floor((now - TEN_MIN_MS) / ONE_SECOND) * ONE_SECOND;

    // Both sources are 1s granularity — merge into a single 1s lookup
    const dataMap = new Map<number, HostMetricPoint>();
    for (const point of [...(historicalData ?? []), ...(streamData ?? [])]) {
      const t =
        Math.floor(new Date(point.recorded_at).getTime() / ONE_SECOND) *
        ONE_SECOND;
      dataMap.set(t, point);
    }

    // Start from the oldest real data point so there's no empty void on the
    // left, capped at the 10-minute live window
    const oldestDataTs =
      dataMap.size > 0 ? Math.min(...dataMap.keys()) : maxStart;
    const start = Math.max(oldestDataTs, maxStart);

    // Generate grid at 1s resolution, using null for missing slots so
    // uPlot spans gaps instead of dropping to zero between real data points
    const grid: HostMetricPoint[] = [];
    for (let t = start; t <= now; t += ONE_SECOND) {
      grid.push(
        dataMap.get(t) ?? {
          recorded_at: new Date(t).toISOString(),
          cpu_percent: null,
          memory_used: null,
          memory_total: null,
          disk_used: null,
          disk_total: null,
          swap_used: null,
          swap_total: null,
        },
      );
    }
    return grid;
  }, [historicalData, streamData]);

  // Detail plots the custom window when filtered, else the live grid.
  const detailMetrics = customRange ? (rangeData ?? []) : liveMetrics;

  // Most recent live point, for the Overview metric cards.
  const latest = useMemo(
    () => [...liveMetrics].reverse().find((p) => p.cpu_percent != null),
    [liveMetrics],
  );

  const { data: settings, isLoading: settingsLoading } = useSettings();
  const updateSettings = useUpdateSettings();

  const handleSaveRetention = (key: string, value: string) => {
    toast.promise(updateSettings.mutateAsync([{ key, value }]), {
      loading: "Saving...",
      success: "Retention setting saved",
      error: "Failed to save retention setting",
    });
  };

  const currentInstanceName =
    settings?.find((s) => s.key === "instance_name")?.value ?? "";

  // Server IP: an override for the address domains must point at (the DNS/TLS
  // precheck baseline). Blank falls back to autodetect; the detected value is
  // shown as the placeholder so an empty field still tells the operator the IP.
  const { data: serverIP } = useServerIP();
  const currentServerIp =
    settings?.find((s) => s.key === "public_ip")?.value ?? "";

  // In a custom window, format x-axis labels with date+time when the span exceeds
  // a day; otherwise time-only keeps short windows readable.
  const chartRange = useMemo(() => {
    if (!customRange) return "1h";
    const spanMs =
      new Date(customRange.to).getTime() - new Date(customRange.from).getTime();
    return spanMs > 24 * 60 * 60 * 1000 ? "7d" : "24h";
  }, [customRange]);

  // Overview is always live; Detail is live unless a custom window is applied.
  const showingLive = hostView === "overview" || customRange === null;

  return (
    <div className="space-y-6">
      <PageHeader
        icon={<ServerIcon className="size-5" />}
        title="Server"
        description="Platform health, resources, and maintenance."
      />

      <PageTabs
        ariaLabel="Server views"
        items={SERVER_TABS}
        value={activeTab}
        onValueChange={setTab}
      />

      {activeTab === "backups" ? (
        <SystemBackupsPanel />
      ) : activeTab === "configuration" ? (
        <div className="space-y-8">
          <div className="space-y-4">
            <p className="text-text-faint text-[10.5px] font-semibold tracking-wider uppercase">
              Platform
            </p>

            <Card>
              <CardHeader>
                <CardTitle className="flex items-center gap-2">
                  <DownloadIcon aria-hidden="true" className="size-4" />
                  Updates
                </CardTitle>
              </CardHeader>
              <CardContent>
                <UpdateSection />
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle className="flex items-center gap-2">
                  <ServerIcon aria-hidden="true" className="size-4" />
                  Instance
                </CardTitle>
              </CardHeader>
              <CardContent className="space-y-2">
                {settingsLoading ? (
                  <div className="max-w-md space-y-3">
                    <Skeleton className="h-9 w-full" />
                    <Skeleton className="h-9 w-full" />
                  </div>
                ) : (
                  // Keyed on the loaded value, not a pre-load placeholder —
                  // mounting before settings resolves would key on "" first,
                  // then remount when the real value arrives.
                  <>
                    <InstanceNameField
                      key={currentInstanceName}
                      currentValue={currentInstanceName}
                      updateSettings={updateSettings}
                    />
                    <ServerIpField
                      key={currentServerIp}
                      currentValue={currentServerIp}
                      placeholder={serverIP?.effective || "Auto-detect"}
                      updateSettings={updateSettings}
                    />
                  </>
                )}

                <div className="border-t pt-4">
                  <DashboardDomainSection />
                </div>
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle className="flex items-center gap-2">
                  <ClockIcon aria-hidden="true" className="size-4" />
                  Metrics Retention
                </CardTitle>
              </CardHeader>
              <CardContent className="space-y-4">
                <div className="grid gap-4 sm:grid-cols-2">
                  <RetentionRow
                    field={HOST_METRICS_FIELD}
                    presets={HOST_METRICS_PRESETS}
                    settings={settings}
                    disabled={updateSettings.isPending}
                    onSave={handleSaveRetention}
                  />
                  {RETENTION_FIELDS.map((field) => (
                    <RetentionRow
                      key={field.key}
                      field={field}
                      presets={RETENTION_PRESETS}
                      settings={settings}
                      disabled={updateSettings.isPending}
                      onSave={handleSaveRetention}
                    />
                  ))}
                </div>
                <p className="text-muted-foreground border-t pt-3 text-xs">
                  Host metrics are stored at 1-second granularity and pruned
                  hourly to the selected window. Logs are pruned daily.
                </p>
              </CardContent>
            </Card>
          </div>

          <div className="space-y-4">
            <p className="text-text-faint text-[10.5px] font-semibold tracking-wider uppercase">
              Operations
            </p>

            <Card>
              <CardHeader>
                <CardTitle className="flex items-center gap-2">
                  <MailIcon aria-hidden="true" className="size-4" />
                  Email (SMTP)
                </CardTitle>
              </CardHeader>
              <CardContent>
                <SmtpSection />
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle className="flex items-center gap-2">
                  <WrenchIcon aria-hidden="true" className="size-4" />
                  Maintenance
                </CardTitle>
              </CardHeader>
              <CardContent>
                <MaintenanceSection />
              </CardContent>
            </Card>
          </div>
        </div>
      ) : isLoading ? (
        <div className="grid grid-cols-2 gap-4 md:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <Card key={i}>
              <CardHeader className="pb-2">
                <div className="bg-muted h-4 w-20 animate-pulse rounded" />
              </CardHeader>
              <CardContent>
                <div className="bg-muted h-8 w-12 animate-pulse rounded" />
              </CardContent>
            </Card>
          ))}
        </div>
      ) : metrics ? (
        <div className="space-y-6">
          <Card>
            <CardHeader>
              <div className="flex flex-wrap items-center justify-between gap-3">
                <CardTitle className="flex items-center gap-2">
                  <Activity aria-hidden="true" className="size-4" />
                  Host Metrics
                </CardTitle>
                <div className="flex items-center gap-2">
                  {/* The time filter only applies to Detail; Overview stays live. */}
                  {hostView === "detail" && (
                    <HostRangeControl
                      value={customRange}
                      onChange={setCustomRange}
                    />
                  )}
                  <ToggleGroup
                    variant="outline"
                    size="sm"
                    value={[hostView]}
                    onValueChange={(v) =>
                      v.length > 0 && setHostView(v[0] as HostMetricsView)
                    }
                    aria-label="Host metrics view"
                  >
                    <ToggleGroupItem value="overview">Overview</ToggleGroupItem>
                    <ToggleGroupItem value="detail">Detail</ToggleGroupItem>
                  </ToggleGroup>
                  {showingLive ? (
                    <LiveIndicator
                      active={streamConnected}
                      idleLabel="Connecting…"
                    />
                  ) : (
                    <Badge variant="outline">Snapshot</Badge>
                  )}
                </div>
              </div>
            </CardHeader>
            <CardContent className="space-y-6">
              {hostView === "overview" ? (
                latest ? (
                  <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
                    <HostMetricCard
                      label="CPU"
                      icon={<CpuIcon className="size-3.5" />}
                      value={`${(latest.cpu_percent ?? 0).toFixed(0)}%`}
                      percent={latest.cpu_percent ?? 0}
                      values={liveMetrics.map((p) => p.cpu_percent)}
                      color={CPU_COLOR}
                    />
                    <HostMetricCard
                      label="Memory"
                      icon={<MemoryStickIcon className="size-3.5" />}
                      value={formatBytes(latest.memory_used)}
                      subline={
                        latest.swap_total
                          ? `Swap ${formatBytes(latest.swap_used)}`
                          : "No swap"
                      }
                      percent={pct(latest.memory_used, latest.memory_total)}
                      values={liveMetrics.map((p) => p.memory_used)}
                      color={MEM_COLOR}
                    />
                    <HostMetricCard
                      label="Disk"
                      icon={<HardDriveIcon className="size-3.5" />}
                      value={formatBytes(latest.disk_used)}
                      percent={pct(latest.disk_used, latest.disk_total)}
                      values={liveMetrics.map((p) => p.disk_used)}
                      color={DISK_COLOR}
                    />
                  </div>
                ) : (
                  <HostMetricsEmpty live />
                )
              ) : detailMetrics.length > 0 ? (
                <div className="grid grid-cols-1 gap-6 md:grid-cols-2">
                  <HostChart
                    title="CPU Usage (%)"
                    data={detailMetrics}
                    dataKey="cpu_percent"
                    range={chartRange}
                    color={CPU_COLOR}
                    formatter={(v: number) => `${v.toFixed(1)}%`}
                    domain={[0, 100]}
                  />
                  <HostChart
                    title="Memory Usage"
                    data={detailMetrics}
                    dataKey="memory_used"
                    range={chartRange}
                    color={MEM_COLOR}
                    formatter={(v: number) => formatBytes(v)}
                    domain={[
                      0,
                      Math.max(
                        ...detailMetrics.map((m) => m.memory_total ?? 0),
                      ),
                    ]}
                  />
                  {detailMetrics.some((m) => (m.swap_total ?? 0) > 0) && (
                    <HostChart
                      title="Swap Usage"
                      data={detailMetrics}
                      dataKey="swap_used"
                      range={chartRange}
                      color={SWAP_COLOR}
                      formatter={(v: number) => formatBytes(v)}
                      domain={[
                        0,
                        Math.max(
                          ...detailMetrics.map((m) => m.swap_total ?? 0),
                        ),
                      ]}
                    />
                  )}
                  <HostChart
                    title="Disk Usage"
                    data={detailMetrics}
                    dataKey="disk_used"
                    range={chartRange}
                    color={DISK_COLOR}
                    formatter={(v: number) => formatBytes(v)}
                    domain={[
                      0,
                      Math.max(...detailMetrics.map((m) => m.disk_total ?? 0)),
                    ]}
                  />
                </div>
              ) : (
                <HostMetricsEmpty live={customRange === null} />
              )}
            </CardContent>
          </Card>

          <div className="grid grid-cols-2 gap-4 md:grid-cols-4">
            <StatCard title="Projects" value={metrics.projects} />
            <StatCard
              title="Applications"
              value={metrics.applications}
              subtitle={`${metrics.containers.by_type?.application?.running ?? 0} running`}
            />
            <StatCard title="Databases" value={metrics.databases} />
            <StatCard
              title="Deployments"
              value={metrics.deployments}
              subtitle="all time"
            />
          </div>

          <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
            <Card>
              <CardHeader>
                <CardTitle className="flex items-center gap-2">
                  <ContainerIcon aria-hidden="true" className="size-4" />
                  Containers
                </CardTitle>
              </CardHeader>
              <CardContent className="space-y-4">
                <div className="flex items-center gap-4">
                  <ContainerStat
                    value={metrics.containers.running}
                    label="Running"
                    variant="default"
                  />
                  <ContainerStat
                    value={metrics.containers.stopped}
                    label="Stopped"
                    variant="secondary"
                  />
                  <ContainerStat
                    value={metrics.containers.error ?? 0}
                    label="Error"
                    variant={
                      (metrics.containers.error ?? 0) > 0
                        ? "destructive"
                        : "outline"
                    }
                  />
                </div>
                {Object.keys(metrics.containers.by_type ?? {}).length > 0 && (
                  <div className="space-y-1.5 border-t pt-3">
                    {Object.entries(metrics.containers.by_type ?? {}).map(
                      ([type, c]) => (
                        <div
                          key={type}
                          className="flex items-center justify-between text-sm"
                        >
                          <span className="text-muted-foreground capitalize">
                            {type}
                          </span>
                          <span className="font-mono">
                            {c.running}
                            <span className="text-text-faint">/{c.total}</span>
                          </span>
                        </div>
                      ),
                    )}
                  </div>
                )}
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <div className="flex items-center justify-between">
                  <CardTitle className="flex items-center gap-2">
                    <PowerIcon aria-hidden="true" className="size-4" />
                    Services
                  </CardTitle>
                  {services && (
                    <Badge
                      variant={
                        services.healthy === services.total
                          ? "default"
                          : "destructive"
                      }
                    >
                      {services.healthy}/{services.total} healthy
                    </Badge>
                  )}
                </div>
              </CardHeader>
              <CardContent className="space-y-3">
                {services?.services.map((s) => (
                  <div
                    key={s.name}
                    className="flex items-center justify-between"
                  >
                    <div>
                      <p className="text-sm font-medium">{s.name}</p>
                      <p className="text-muted-foreground text-sm">
                        {s.description}
                      </p>
                    </div>
                    <StatusPill
                      status={s.status === "running" ? "running" : "error"}
                      label={s.status === "running" ? "Healthy" : "Down"}
                    />
                  </div>
                ))}
                {!services && (
                  <p className="text-muted-foreground text-sm">
                    Checking services…
                  </p>
                )}
              </CardContent>
            </Card>
          </div>
        </div>
      ) : null}
    </div>
  );
}

function HostMetricsEmpty({ live }: { live: boolean }) {
  return (
    <p className="text-muted-foreground py-8 text-center text-sm">
      {live
        ? "No metrics data available yet. Data is collected every second."
        : "No metrics in the selected window."}
    </p>
  );
}

/** datetime-local string (local time, minute precision) for a Date. */
function toLocalInputValue(d: Date) {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(
    d.getHours(),
  )}:${pad(d.getMinutes())}`;
}

function formatRangeLabel(iso: string) {
  return formatDateTimeShort(iso);
}

function HostRangeControl({
  value,
  onChange,
}: {
  value: CustomRange | null;
  onChange: (next: CustomRange | null) => void;
}) {
  const [open, setOpen] = useState(false);

  const body = useDialogBody(open, value);

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        render={<Button variant="outline" size="sm" className="gap-1.5" />}
      >
        <CalendarIcon className="size-3.5 shrink-0" />
        <span className="max-w-[200px] truncate">
          {value
            ? `${formatRangeLabel(value.from)} – ${formatRangeLabel(value.to)}`
            : "Live (10m)"}
        </span>
      </PopoverTrigger>
      {/* Rendered fresh each time the popover opens, so its form picks up
          `value` as-of that moment without needing an imperative reset — and
          kept through the close animation; see useDialogBody. */}
      <RangeForm
        key={body.key}
        value={body.target}
        onApply={(next) => {
          onChange(next);
          setOpen(false);
        }}
        onLive={() => {
          onChange(null);
          setOpen(false);
        }}
      />
    </Popover>
  );
}

function RangeForm({
  value,
  onApply,
  onLive,
}: {
  value: CustomRange | null;
  onApply: (next: CustomRange) => void;
  onLive: () => void;
}) {
  const now = new Date();
  const start = value
    ? new Date(value.from)
    : new Date(now.getTime() - TEN_MIN_MS);
  const end = value ? new Date(value.to) : now;

  const form = useForm({
    defaultValues: {
      from: toLocalInputValue(start),
      to: toLocalInputValue(end),
    },
    onSubmit: ({ value: v }) => {
      onApply({
        from: new Date(v.from).toISOString(),
        to: new Date(v.to).toISOString(),
      });
    },
  });

  return (
    <PopoverContent align="end" className="w-72 space-y-3">
      <form
        onSubmit={(e) => {
          e.preventDefault();
          e.stopPropagation();
          form.handleSubmit();
        }}
        className="contents"
      >
        <form.Field
          name="from"
          validators={{
            onChange: ({ value }) =>
              value ? undefined : "Start time is required",
          }}
          children={(field) => {
            const error = fieldError(field.state.meta.errors);
            return (
              <Field data-invalid={!!error} className="gap-1.5">
                <FieldLabel htmlFor="metric-from" className="text-xs">
                  Start
                </FieldLabel>
                <Input
                  id="metric-from"
                  type="datetime-local"
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value)}
                  aria-invalid={!!error}
                />
                {error && <FieldError>{error}</FieldError>}
              </Field>
            );
          }}
        />
        <form.Field
          name="to"
          validators={{
            onChangeListenTo: ["from"],
            onChange: ({ value: v, fieldApi }) => {
              if (!v) return "End time is required";
              const fromVal = fieldApi.form.state.values.from;
              if (fromVal && new Date(fromVal) >= new Date(v)) {
                return "Start time must be before end time";
              }
              return undefined;
            },
          }}
          children={(field) => {
            const error = fieldError(field.state.meta.errors);
            return (
              <Field data-invalid={!!error} className="gap-1.5">
                <FieldLabel htmlFor="metric-to" className="text-xs">
                  End
                </FieldLabel>
                <Input
                  id="metric-to"
                  type="datetime-local"
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value)}
                  aria-invalid={!!error}
                />
                {error && <FieldError>{error}</FieldError>}
              </Field>
            );
          }}
        />
        <div className="flex items-center justify-between gap-2 pt-1">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={onLive}
            disabled={!value}
          >
            Live
          </Button>
          <Button type="submit" size="sm">
            Apply
          </Button>
        </div>
      </form>
    </PopoverContent>
  );
}

function StatCard({
  title,
  value,
  subtitle,
}: {
  title: string;
  value: number;
  subtitle?: string;
}) {
  return (
    <Card>
      <CardHeader className="pb-2">
        <p className="text-muted-foreground text-sm font-medium">{title}</p>
      </CardHeader>
      <CardContent>
        <p className="text-3xl font-bold">{value}</p>
        {subtitle && <p className="text-text-faint mt-1 text-xs">{subtitle}</p>}
      </CardContent>
    </Card>
  );
}

function ContainerStat({
  value,
  label,
  variant,
}: {
  value: number;
  label: string;
  variant: "default" | "secondary" | "outline" | "destructive";
}) {
  return (
    <div className="flex items-center gap-2">
      <Badge variant={variant}>{value}</Badge>
      <span className="text-muted-foreground text-sm">{label}</span>
    </div>
  );
}

function HostMetricCard({
  label,
  icon,
  value,
  subline,
  percent,
  values,
  color,
}: {
  label: string;
  icon?: ReactNode;
  value: string;
  // Optional second line under the headline — e.g. swap under RAM. Kept subdued
  // precisely so it reads as secondary, never summed into the headline number.
  subline?: ReactNode;
  percent: number;
  values: (number | null)[];
  color: string;
}) {
  const s = loadStatus(percent);
  return (
    <MetricCard
      label={label}
      icon={icon}
      aside={
        <span className={cn("text-xs font-medium capitalize", s.className)}>
          {s.label}
        </span>
      }
    >
      {/* Baseline row, not a second line: as its own <p> the subline made the
          card one line taller than its neighbours, so Memory never lined up
          with CPU and Disk. Still visibly secondary — subdued, small, and
          labelled — because RAM and swap are two separate figures and must
          never read as one summed number. */}
      <div className="mt-1 flex items-baseline gap-2">
        <p className="font-mono text-2xl font-semibold">{value}</p>
        {subline && (
          <span className="text-text-faint font-mono text-xs">{subline}</span>
        )}
      </div>
      <Sparkline className="mt-2" height={36} values={values} color={color} />
    </MetricCard>
  );
}

function RetentionRow({
  field,
  presets,
  settings,
  disabled,
  onSave,
}: {
  field: RetentionField;
  presets: RetentionPreset[];
  settings: SettingEntry[] | undefined;
  disabled: boolean;
  onSave: (key: string, value: string) => void;
}) {
  const current =
    settings?.find((s) => s.key === field.key)?.value ?? field.fallback;

  // Surface the current value even if it isn't one of the presets.
  const options = [...presets];
  if (!options.some((p) => p.value === current)) {
    options.unshift({ label: current, value: current });
  }
  const currentLabel =
    options.find((p) => p.value === current)?.label ?? current;

  return (
    <div className="flex items-center justify-between gap-4">
      <div>
        <p className="text-sm font-medium">{field.label}</p>
        <p className="text-muted-foreground text-xs">{field.desc}</p>
      </div>
      <Select
        value={current}
        onValueChange={(v) => v && v !== current && onSave(field.key, v)}
        disabled={disabled}
      >
        <SelectTrigger className="w-32">
          {/* Render the label explicitly: base-ui's items-based lookup
              mis-resolves dynamically-prepended non-preset values. */}
          <span className="truncate capitalize">{currentLabel}</span>
        </SelectTrigger>
        <SelectContent>
          {options.map((p) => (
            <SelectItem
              key={p.value}
              value={p.value}
              icon={<ClockIcon />}
              className="capitalize"
            >
              {p.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}

function HostChart({
  title,
  data,
  dataKey,
  range,
  color,
  formatter,
  domain,
}: {
  title: string;
  data: HostMetricPoint[];
  dataKey: string;
  range: string;
  color: string;
  formatter: (value: number) => string;
  domain?: [number, number];
}) {
  return (
    <div>
      <p className="mb-2 text-sm font-medium">{title}</p>
      <UPlotAreaChart
        timestamps={data.map((p) => new Date(p.recorded_at).getTime())}
        values={data.map(
          (p) => p[dataKey as keyof HostMetricPoint] as number | null,
        )}
        color={color}
        yFormatter={formatter}
        xFormatter={(ts) => formatTime(new Date(ts).toISOString(), range)}
        yDomain={domain}
        height={200}
        syncKey="host-metrics-detail"
      />
    </div>
  );
}
