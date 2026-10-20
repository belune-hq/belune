import {
  useState,
  type FocusEvent,
  type PointerEvent,
  type ReactNode,
} from "react";
import { createFileRoute, notFound } from "@tanstack/react-router";
import type { ColumnDef } from "@tanstack/react-table";
import {
  Check,
  MoreHorizontal,
  Play,
  Plus,
  RotateCcw,
  ScrollText,
  Square,
  Trash2,
} from "lucide-react";
import { BeluneLogo } from "@/lib/components/belune-logo";
import { cn } from "@/lib/utils";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { CodeEditor } from "@/components/ui/code-editor";
import { DataTable, buildActionColumnDef } from "@/components/ui/data-table";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { FieldError } from "@/components/ui/field";
import { IconAction } from "@/components/ui/icon-action";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { LiveIndicator } from "@/components/ui/live-indicator";
import {
  SegmentedControl,
  SegmentedControlItem,
} from "@/components/ui/segmented-control";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import { Sparkline } from "@/components/ui/sparkline";
import { StatusBar } from "@/components/ui/status-bar";
import { StatusPill } from "@/components/ui/status-pill";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";

/**
 * Dev-only theme matrix: every primitive rendered in all four combinations of
 * accent × mode on one page. Exists so a colour decision can be checked in the
 * one combination it usually fails in (emerald + dark, where `--brand-fg` is
 * near-black) without toggling the app back and forth and remembering.
 *
 * The quadrants are real theme scopes (`.light`/`.dark` + `data-accent`), so
 * they render through the same tokens and the same `dark:` utilities as the
 * app — not a copy of the stylesheet.
 *
 * Overlays are the one thing a scope cannot contain: Tooltip, Select's popup,
 * DropdownMenu and Dialog all portal to <body>, so they take the PAGE theme,
 * not their quadrant's. Rather than leave them out, each quadrant sets the
 * page theme to its own while the pointer or focus is inside it and restores
 * it on the way out — so an overlay opened from a quadrant renders under that
 * quadrant's theme. The matrix itself never depends on the page theme, so
 * the quadrants do not move while this happens.
 *
 * Gated in beforeLoad rather than by omission so the URL 404s in a production
 * build; the component itself is code-split by the router plugin and never
 * fetched there.
 */
export const Route = createFileRoute("/dev/theme")({
  beforeLoad: () => {
    if (!import.meta.env.DEV) throw notFound();
  },
  component: ThemeMatrixPage,
});

type Mode = "light" | "dark";
type Accent = "violet" | "emerald";

const QUADRANTS: { mode: Mode; accent: Accent }[] = [
  { mode: "light", accent: "violet" },
  { mode: "dark", accent: "violet" },
  { mode: "light", accent: "emerald" },
  { mode: "dark", accent: "emerald" },
];

function ThemeMatrixPage() {
  return (
    <div className="bg-background text-foreground min-h-screen p-4 md:p-6">
      <header className="mb-4 flex flex-wrap items-baseline justify-between gap-2">
        <h1 className="text-lg font-semibold">Theme matrix</h1>
        <p className="text-muted-foreground text-sm">
          Development only. Overlays (tooltips, menus, selects) portal to the
          page, so the page follows whichever quadrant the pointer is in.
        </p>
      </header>
      <div className="grid gap-4 xl:grid-cols-2">
        {QUADRANTS.map(({ mode, accent }) => (
          <Quadrant key={`${mode}-${accent}`} mode={mode} accent={accent} />
        ))}
      </div>
    </div>
  );
}

/** What <html> looked like before a quadrant borrowed the page theme. */
interface PageTheme {
  className: string;
  accent: string | undefined;
  colorScheme: string;
}

// One snapshot for the page, not one per quadrant: moving straight from one
// quadrant into another borrows twice and restores once, and the restore must
// land on what the page had before either of them.
let original: PageTheme | null = null;

// next-themes owns <html>'s class and the accent store owns data-accent, but
// neither watches for outside changes, so borrowing and restoring by hand is
// safe as long as the snapshot is eventually restored.
function borrowPageTheme(mode: Mode, accent: Accent) {
  const html = document.documentElement;
  original ??= {
    className: html.className,
    accent: html.dataset.accent,
    colorScheme: html.style.colorScheme,
  };
  html.className = mode;
  if (accent === "emerald") html.dataset.accent = "emerald";
  else delete html.dataset.accent;
  html.style.colorScheme = mode;
}

function restorePageTheme() {
  if (!original) return;
  const html = document.documentElement;
  html.className = original.className;
  if (original.accent) html.dataset.accent = original.accent;
  else delete html.dataset.accent;
  html.style.colorScheme = original.colorScheme;
  original = null;
}

/**
 * Restore unless some quadrant still holds the pointer or focus. A menu or
 * select popup is outside its quadrant, so the pointer has usually already
 * left by the time the popup closes — the leave was ignored (see Quadrant),
 * and this is the deferred restore for it.
 */
function settlePageTheme() {
  const held = document.querySelector(
    "section[data-quadrant]:hover, section[data-quadrant]:focus-within",
  );
  if (!held) restorePageTheme();
}

function Quadrant({ mode, accent }: { mode: Mode; accent: Accent }) {
  const id = `${mode}-${accent}`;
  const borrow = () => borrowPageTheme(mode, accent);
  // Leaving for an open popup is not leaving: the popup is the quadrant's own
  // overlay, and settlePageTheme runs when it closes.
  const onPointerLeave = (e: PointerEvent<HTMLElement>) => {
    if (!e.currentTarget.querySelector("[data-popup-open]")) restorePageTheme();
  };
  // Keyboard users open tooltips and menus with focus, not hover; only restore
  // when focus actually leaves the quadrant, not when it moves inside it.
  const onBlur = (e: FocusEvent<HTMLElement>) => {
    if (!e.currentTarget.contains(e.relatedTarget)) restorePageTheme();
  };

  return (
    <section
      data-quadrant
      onPointerEnter={borrow}
      onPointerLeave={onPointerLeave}
      onFocus={borrow}
      onBlur={onBlur}
      // The app only ever sets data-accent for emerald (violet is the absence
      // of it), and the `.light`/`.dark` blocks each re-declare the violet
      // brand, so a violet quadrant needs no attribute to override an emerald
      // page.
      className={cn(
        mode,
        "bg-background text-foreground border-border rounded-xl border p-5 shadow-sm",
      )}
      data-accent={accent === "emerald" ? "emerald" : undefined}
      style={{ colorScheme: mode }}
      aria-labelledby={`${id}-title`}
    >
      <div className="mb-4 flex items-center justify-between">
        <h2 id={`${id}-title`} className="font-mono text-sm font-medium">
          {accent} · {mode}
        </h2>
        <span
          aria-hidden="true"
          className="text-brand-fg grid size-8 place-items-center rounded-lg shadow-sm"
          style={{
            background:
              "linear-gradient(140deg, var(--brand), var(--brand-press))",
          }}
        >
          <BeluneLogo className="size-6" />
        </span>
      </div>
      <Showcase id={id} />
    </section>
  );
}

const SPARK = [3, 5, 4, 8, 6, 9, 7, 11, 10, 12];

interface FakeService {
  id: string;
  name: string;
  kind: string;
  status: string;
  cpu: string;
}

const SERVICES: FakeService[] = [
  {
    id: "web",
    name: "web",
    kind: "Application",
    status: "running",
    cpu: "12%",
  },
  {
    id: "worker",
    name: "worker",
    kind: "Application",
    status: "building",
    cpu: "—",
  },
  {
    id: "db",
    name: "postgres",
    kind: "Database",
    status: "running",
    cpu: "3%",
  },
  { id: "cache", name: "redis", kind: "Database", status: "stopped", cpu: "—" },
  {
    id: "cron",
    name: "reports",
    kind: "Application",
    status: "failed",
    cpu: "—",
  },
];

// The two action styles the app actually uses in a row: the icon actions of
// the project services table, and the labelled small buttons of the team page.
const SERVICE_COLUMNS: ColumnDef<FakeService>[] = [
  {
    id: "name",
    header: "Service",
    accessorKey: "name",
    cell: ({ row: { original: svc } }) => (
      <div className="min-w-0">
        <div className="truncate font-medium">{svc.name}</div>
        <div className="text-text-faint text-xs">{svc.kind}</div>
      </div>
    ),
  },
  {
    id: "status",
    header: "Status",
    accessorKey: "status",
    cell: ({ row: { original: svc } }) => <StatusPill status={svc.status} />,
  },
  {
    id: "cpu",
    header: "CPU",
    accessorKey: "cpu",
    meta: { headerClassName: "text-right", className: "text-right" },
    cell: ({ row: { original: svc } }) => (
      <span className="text-muted-foreground tabular-nums">{svc.cpu}</span>
    ),
  },
  buildActionColumnDef<FakeService>({
    meta: { headerClassName: "text-right", className: "text-right" },
    cell: ({ row: { original: svc } }) => (
      <div className="flex items-center justify-end gap-1">
        {svc.status === "running" ? (
          <IconAction label="Stop" onClick={noop} destructive>
            <Square aria-hidden="true" className="size-4" />
          </IconAction>
        ) : (
          <IconAction label="Start" onClick={noop}>
            <Play aria-hidden="true" className="size-4" />
          </IconAction>
        )}
        <IconAction label="Restart" onClick={noop}>
          <RotateCcw aria-hidden="true" className="size-4" />
        </IconAction>
        <IconAction label="Logs" onClick={noop}>
          <ScrollText aria-hidden="true" className="size-4" />
        </IconAction>
        <IconAction label="Delete" onClick={noop} destructive>
          <Trash2 aria-hidden="true" className="size-4" />
        </IconAction>
        <DropdownMenu
          onOpenChange={(open) => {
            if (!open) settlePageTheme();
          }}
        >
          <DropdownMenuTrigger
            render={
              <Button variant="ghost" size="icon" aria-label="More actions" />
            }
          >
            <MoreHorizontal aria-hidden="true" className="size-4" />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-40">
            <DropdownMenuItem>Open</DropdownMenuItem>
            <DropdownMenuItem>Redeploy</DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive">Delete</DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    ),
  }),
];

function noop() {}

// One of every JSON token type, so each highlight colour is on screen at once.
const SAMPLE_JSON = `{
  "window": "1s",
  "max_events": 10,
  "enabled": true,
  "note": null,
  "ranges": ["10.0.0.0/8", "192.168.0.0/16"]
}`;

function Showcase({ id }: { id: string }) {
  const [segment, setSegment] = useState("all");
  const [compactJson, setCompactJson] = useState(
    '{"window":"1s","max_events":10}',
  );
  const [blockJson, setBlockJson] = useState(SAMPLE_JSON);

  return (
    <div className="space-y-5">
      <Row title="Buttons">
        <Button>Deploy</Button>
        <Button variant="secondary">Secondary</Button>
        <Button variant="outline">Outline</Button>
        <Button variant="ghost">Ghost</Button>
        <Button variant="destructive">Destructive</Button>
        <Button variant="destructive-solid">Delete</Button>
        <Button variant="link">Link</Button>
        <Button size="sm">
          <Plus data-icon="inline-start" /> Small
        </Button>
        <Button size="icon" variant="outline" aria-label="Delete">
          <Trash2 />
        </Button>
        <Button disabled>Disabled</Button>
      </Row>

      <Row title="Text on the accent">
        <span className="bg-primary text-primary-foreground rounded-md px-2.5 py-1 text-sm">
          text-brand-fg
        </span>
        {/* Hardcoded white, kept for comparison against text-brand-fg above.
            It currently matches in all four theme+accent combinations —
            dark+emerald only gets there because --brand itself was
            deliberately darkened (see the .dark[data-accent="emerald"]
            comment in index.css) specifically so its foreground could stay
            white — so this won't visibly diverge today. The point survives
            anyway: a future accent or a lighter brand color could need dark
            text again, and a hardcoded span wouldn't know to follow. */}
        <span className="bg-primary rounded-md px-2.5 py-1 text-sm text-white">
          text-white
        </span>
        <span className="text-brand-text text-sm">
          text-brand-text on surface
        </span>
      </Row>

      <Row title="Badges & status">
        <Badge>Default</Badge>
        <Badge variant="secondary">Secondary</Badge>
        <Badge variant="light">Light</Badge>
        <Badge variant="outline">Outline</Badge>
        <Badge variant="destructive">Destructive</Badge>
        <StatusPill status="running" />
        <StatusPill status="building" />
        <StatusPill status="failed" />
        <StatusPill status="stopped" />
        <LiveIndicator active />
        <LiveIndicator active={false} />
      </Row>

      <Row title="Form controls">
        <div className="grid w-full gap-3 sm:grid-cols-2">
          <div className="space-y-2">
            <Label htmlFor={`${id}-name`}>Name</Label>
            <Input id={`${id}-name`} placeholder="my-service" />
          </div>
          <div className="space-y-2">
            <Label htmlFor={`${id}-invalid`}>Port</Label>
            <Input
              id={`${id}-invalid`}
              defaultValue="99999"
              aria-invalid
              aria-describedby={`${id}-invalid-err`}
            />
            <FieldError id={`${id}-invalid-err`}>
              Must be between 1 and 65535.
            </FieldError>
          </div>
          <div className="space-y-2">
            <Label htmlFor={`${id}-disabled`} aria-disabled>
              Disabled
            </Label>
            <Input id={`${id}-disabled`} disabled placeholder="Read only" />
          </div>
          <div className="space-y-2">
            <Label htmlFor={`${id}-notes`}>Notes</Label>
            <Textarea id={`${id}-notes`} rows={2} placeholder="Optional" />
          </div>
          {/* Compact is the real call site (route feature config): it sits
              beside an Input on purpose, to show the two are the same height.
              Block is what the multi-line fields will use. */}
          <div className="space-y-2">
            <Label id={`${id}-json-label`}>Config (compact)</Label>
            <CodeEditor
              compact
              language="json"
              value={compactJson}
              onChange={setCompactJson}
              aria-labelledby={`${id}-json-label`}
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor={`${id}-height`}>Input (same height)</Label>
            <Input id={`${id}-height`} placeholder="my-service" />
          </div>
          <div className="space-y-2 sm:col-span-2">
            <Label id={`${id}-block-label`}>Config (block)</Label>
            <CodeEditor
              language="json"
              value={blockJson}
              onChange={setBlockJson}
              placeholder="Empty shows this placeholder"
              aria-labelledby={`${id}-block-label`}
            />
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-5">
          <Label className="gap-2">
            <Checkbox defaultChecked /> Checked
          </Label>
          <Label className="gap-2">
            <Checkbox /> Unchecked
          </Label>
          <Label className="gap-2">
            <Switch defaultChecked /> On
          </Label>
          <Label className="gap-2">
            <Switch /> Off
          </Label>
          <Label className="gap-2" aria-disabled>
            <Switch disabled defaultChecked /> Disabled
          </Label>
        </div>
        <Select
          defaultValue="postgres"
          onOpenChange={(open) => {
            if (!open) settlePageTheme();
          }}
        >
          <SelectTrigger className="w-40" aria-label="Database type">
            <SelectValue placeholder="Select type" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="postgres">Postgres</SelectItem>
            <SelectItem value="mysql">MySQL</SelectItem>
            <SelectItem value="redis">Redis</SelectItem>
          </SelectContent>
        </Select>
        <SegmentedControl value={segment} onValueChange={setSegment} size="sm">
          <SegmentedControlItem value="all">All</SegmentedControlItem>
          <SegmentedControlItem value="running">Running</SegmentedControlItem>
          <SegmentedControlItem value="stopped">Stopped</SegmentedControlItem>
        </SegmentedControl>
        <Tabs defaultValue="overview">
          <TabsList>
            <TabsTrigger value="overview">Overview</TabsTrigger>
            <TabsTrigger value="logs">Logs</TabsTrigger>
            <TabsTrigger value="settings">Settings</TabsTrigger>
          </TabsList>
        </Tabs>
      </Row>

      <Row title="Alerts">
        <div className="grid w-full gap-2">
          <Alert>
            <Check />
            <AlertTitle>Deployed</AlertTitle>
            <AlertDescription>web is serving v42.</AlertDescription>
          </Alert>
          <Alert variant="warning">
            <AlertTitle>Certificate expires in 6 days</AlertTitle>
            <AlertDescription>Renewal is scheduled tonight.</AlertDescription>
          </Alert>
          <Alert variant="destructive">
            <AlertTitle>Build failed</AlertTitle>
            <AlertDescription>Exit code 1 at step 4/9.</AlertDescription>
          </Alert>
        </div>
      </Row>

      <Row title="Table">
        <DataTable
          columns={SERVICE_COLUMNS}
          data={SERVICES}
          getRowId={(svc) => svc.id}
          enableSorting
          className="w-full"
        />
      </Row>

      <Row title="Surfaces">
        <Card className="w-full">
          <CardHeader>
            <CardTitle>Instance</CardTitle>
            <CardDescription>
              Text hierarchy:{" "}
              <span className="text-foreground">foreground</span>,{" "}
              <span className="text-text-muted">muted</span>,{" "}
              <span className="text-text-faint">faint</span>,{" "}
              <span className="text-text-faintest">faintest</span>.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <StatusBar
              segments={[
                { label: "Running", count: 6, className: "bg-status-ready" },
                {
                  label: "Building",
                  count: 2,
                  className: "bg-status-building",
                },
                { label: "Failed", count: 1, className: "bg-status-error" },
              ]}
            />
            <Sparkline values={SPARK} height={32} />
            <Separator />
            <div className="space-y-2">
              <Skeleton className="h-4 w-1/2" />
              <Skeleton className="h-4 w-1/3" />
            </div>
          </CardContent>
        </Card>
      </Row>
    </div>
  );
}

function Row({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div>
      <p className="text-text-faint mb-2 text-[11px] font-medium tracking-wide uppercase">
        {title}
      </p>
      <div className="flex flex-wrap items-center gap-2">{children}</div>
    </div>
  );
}
