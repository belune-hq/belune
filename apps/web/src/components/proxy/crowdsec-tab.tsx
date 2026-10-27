import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import {
  BanIcon,
  BrickWallFireIcon,
  ListCheckIcon,
  ShieldXIcon,
} from "lucide-react";
import { toast } from "sonner";
import { CROWDSEC_SHIELD } from "@/components/proxy/crowdsec-shield";
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
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldError } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusPill } from "@/components/ui/status-pill";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  ALLOW_DURATIONS,
  BAN_DURATIONS,
  autobanEnablePoints,
  bouncerNotice,
  crowdSecEnablePoints,
  describeAutoban,
  describeCrowdSec,
  describeYourAddress,
  isBanTarget,
  crowdSecShield,
  reasonText,
  sourceLabel,
  timeLeft,
  type CrowdSecTone,
} from "@/components/proxy/crowdsec-view";
import {
  useAllowAddress,
  useBanAddress,
  useCrowdSecAllowlist,
  useCrowdSecDecisions,
  useCrowdSecStatus,
  useLiftBan,
  useRemoveAllowed,
  useRotateCrowdSecKey,
  useSetCrowdSecAutoban,
  useSetCrowdSecEnabled,
} from "@/lib/hooks/use-crowdsec";
import { useWafStatus } from "@/lib/hooks/use-waf";
import { cn } from "@/lib/utils";
import { fieldError } from "@/lib/utils/field-error";
import type {
  CrowdSecAllowItem,
  CrowdSecDecision,
  CrowdSecPolicy,
  CrowdSecStatus,
} from "@/lib/types";

const PILL_TONE = {
  neutral: "neutral",
  attention: "building",
  ready: "ready",
  error: "error",
} as const;

const TEXT_TONE: Record<CrowdSecTone, string> = {
  neutral: "text-muted-foreground",
  attention: "text-status-building",
  ready: "text-status-ready",
  error: "text-status-error",
};

/**
 * CrowdSec: bans that Caddy enforces. Unlike the WAF this one acts, so the page
 * is built around the three things that keep it safe to act: the way back (lifting
 * a ban never needs the feature on), the allowlist, and the honest account of
 * whether bans are actually being enforced.
 */
export function CrowdSecTab() {
  const { data: status, isLoading } = useCrowdSecStatus();
  // Reading the bans and the allowlist means running a command in the CrowdSec
  // container; there is nothing to ask when it is not answering.
  const reachable = status?.local_api.reachable ?? false;

  return (
    <div className="space-y-6">
      <StatusCard status={status} loading={isLoading} />
      {status && reachable && (
        <>
          <BansCard status={status} />
          <AllowlistCard status={status} />
        </>
      )}
      {status && <AutobanCard status={status} />}
      <LockoutCard />
    </div>
  );
}

function StatusCard({
  status,
  loading,
}: {
  status: CrowdSecStatus | undefined;
  loading: boolean;
}) {
  const setEnabled = useSetCrowdSecEnabled();
  const rotate = useRotateCrowdSecKey();
  const [confirming, setConfirming] = useState(false);
  const d = status ? describeCrowdSec(status) : null;
  const Icon = CROWDSEC_SHIELD[crowdSecShield(status)];
  const stale = status ? bouncerNotice(status.local_api, new Date()) : null;
  const yours = status
    ? describeYourAddress(
        status.your_ip,
        status.your_ip_protection,
        status.enabled,
      )
    : null;

  const apply = (enabled: boolean) =>
    toast.promise(setEnabled.mutateAsync(enabled), {
      loading: enabled ? "Turning CrowdSec on…" : "Turning CrowdSec off…",
      success: enabled ? "CrowdSec turned on" : "CrowdSec turned off",
      error: (e: Error) => e.message || "Could not change CrowdSec",
    });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2">
          <Icon aria-hidden="true" className="size-4" />
          CrowdSec
          {d && (
            <StatusPill
              status={d.pill.label}
              label={d.pill.label}
              tone={PILL_TONE[d.pill.tone]}
            />
          )}
        </CardTitle>
        <CardDescription>
          Ban addresses from your applications. Caddy turns each ban into a 403.
        </CardDescription>
        {status && d && (
          <CardAction>
            <Switch
              aria-label="Enforce bans with CrowdSec"
              checked={status.enabled}
              disabled={
                setEnabled.isPending || (!status.enabled && !d.canEnable)
              }
              onCheckedChange={(on) =>
                on ? setConfirming(true) : apply(false)
              }
            />
          </CardAction>
        )}
      </CardHeader>
      <CardContent className="space-y-3">
        {loading || !status || !d || !yours ? (
          <Skeleton className="h-10 w-2/3" />
        ) : (
          <>
            <p className="text-muted-foreground text-sm">{d.body}</p>
            {d.notice && (
              <p className={cn("text-sm", TEXT_TONE[d.notice.tone])}>
                {d.notice.text}
              </p>
            )}
            {status.enabled && stale && (
              <Button
                variant="outline"
                size="sm"
                disabled={rotate.isPending}
                onClick={() =>
                  toast.promise(rotate.mutateAsync(), {
                    loading: "Replacing the bouncer's key…",
                    success: "Bouncer key replaced",
                    error: (e: Error) =>
                      e.message || "Could not replace the key",
                  })
                }
              >
                Rotate the bouncer key
              </Button>
            )}
            <p
              className={cn(
                "text-xs",
                yours.tone === "attention"
                  ? TEXT_TONE.attention
                  : "text-text-faint",
              )}
            >
              {yours.text}
            </p>
          </>
        )}
      </CardContent>

      <AlertDialog open={confirming} onOpenChange={setConfirming}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Turn on CrowdSec?</AlertDialogTitle>
            <AlertDialogDescription>
              Before you do, what this does and does not do:
            </AlertDialogDescription>
          </AlertDialogHeader>
          <ul className="text-muted-foreground list-disc space-y-1.5 pl-5 text-sm">
            {crowdSecEnablePoints(
              status?.your_ip ?? "",
              status?.your_ip_protection,
            ).map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => apply(true)}>
              Turn on
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}

function BansCard({ status }: { status: CrowdSecStatus }) {
  const { data, isLoading, error } = useCrowdSecDecisions();
  const rows = data?.decisions ?? [];
  const ban = useBanAddress();

  const form = useForm({
    defaultValues: { target: "", duration: "24h", reason: "" },
    onSubmit: async ({ value, formApi }) => {
      const target = value.target.trim();
      const banned = ban.mutateAsync({
        target,
        duration: value.duration,
        reason: value.reason.trim() || undefined,
      });
      toast.promise(banned, {
        loading: `Banning ${target}…`,
        success: `Banned ${target}`,
        error: (e: Error) => e.message || "Could not ban that address",
      });
      // The form is cleared only by a ban that happened: a refusal leaves what was
      // typed in place to be corrected.
      await banned.then(() => formApi.reset()).catch(() => undefined);
    },
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <BanIcon aria-hidden="true" className="size-4" />
          Bans
        </CardTitle>
        <CardDescription>
          Every ban expires. Lifting one takes effect within a few seconds.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <form
          className="flex flex-wrap items-start gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            e.stopPropagation();
            form.handleSubmit();
          }}
        >
          <form.Field
            name="target"
            validators={{
              onChange: ({ value }) =>
                value === "" || isBanTarget(value)
                  ? undefined
                  : "Enter an IP address or a CIDR range",
              onSubmit: ({ value }) =>
                isBanTarget(value)
                  ? undefined
                  : "Enter an IP address or a CIDR range",
            }}
            children={(field) => {
              const error = fieldError(field.state.meta.errors);
              return (
                <Field data-invalid={!!error} className="w-52">
                  <Input
                    aria-label="Address to ban"
                    placeholder="203.0.113.9 or 203.0.113.0/24"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                  />
                  {error && <FieldError>{error}</FieldError>}
                </Field>
              );
            }}
          />
          <form.Field
            name="duration"
            children={(field) => (
              <Select
                value={field.state.value}
                onValueChange={(v) => field.handleChange(v ?? "24h")}
              >
                <SelectTrigger aria-label="Ban length" className="w-36">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {BAN_DURATIONS.map((d) => (
                    <SelectItem key={d.value} value={d.value}>
                      {d.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          <form.Field
            name="reason"
            validators={{
              onChange: ({ value }) =>
                value.length <= 100 ? undefined : "At most 100 characters",
            }}
            children={(field) => {
              const error = fieldError(field.state.meta.errors);
              return (
                <Field data-invalid={!!error} className="w-56">
                  <Input
                    aria-label="Reason"
                    placeholder="Reason (optional)"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                  />
                  {error && <FieldError>{error}</FieldError>}
                </Field>
              );
            }}
          />
          <Button type="submit" disabled={!status.enabled || ban.isPending}>
            Ban
          </Button>
        </form>
        {!status.enabled && (
          <p className="text-muted-foreground text-sm">
            Turn CrowdSec on to ban an address. Existing bans can be lifted
            either way.
          </p>
        )}

        {isLoading ? (
          <Skeleton className="h-16 w-full" />
        ) : error ? (
          <p className="text-status-error text-sm">
            Could not read the bans: {(error as Error).message}
          </p>
        ) : rows.length === 0 ? (
          <p className="text-muted-foreground text-sm">Nothing is banned.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Address</TableHead>
                <TableHead>Source</TableHead>
                <TableHead>Reason</TableHead>
                <TableHead>Expires in</TableHead>
                <TableHead className="w-0" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((d) => (
                <BanRow key={d.id} decision={d} />
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}

function BanRow({ decision: d }: { decision: CrowdSecDecision }) {
  const lift = useLiftBan();
  return (
    <TableRow>
      <TableCell className="font-mono text-sm">
        {d.value}
        {d.scope === "range" && (
          <Badge variant="outline" className="ml-2">
            range
          </Badge>
        )}
      </TableCell>
      <TableCell>
        <Badge variant={d.source === "waf" ? "secondary" : "outline"}>
          {sourceLabel(d.source)}
        </Badge>
      </TableCell>
      <TableCell className="max-w-xs truncate" title={d.reason}>
        {reasonText(d.reason)}
      </TableCell>
      <TableCell
        className="text-muted-foreground whitespace-nowrap"
        title={new Date(d.expires_at).toLocaleString()}
      >
        {timeLeft(d.expires_at)}
      </TableCell>
      <TableCell>
        <Button
          variant="ghost"
          size="sm"
          aria-label={`Lift the ban on ${d.value}`}
          disabled={lift.isPending}
          onClick={() =>
            toast.promise(lift.mutateAsync(d.value), {
              loading: `Lifting the ban on ${d.value}…`,
              success: `Lifted the ban on ${d.value}`,
              error: (e: Error) => e.message || "Could not lift that ban",
            })
          }
        >
          Lift
        </Button>
      </TableCell>
    </TableRow>
  );
}

function AllowlistCard({ status }: { status: CrowdSecStatus }) {
  const { data, isLoading, error } = useCrowdSecAllowlist();
  const items = data?.items ?? [];
  const allow = useAllowAddress();

  const form = useForm({
    defaultValues: { target: "", duration: "30d", note: "" },
    onSubmit: async ({ value, formApi }) => {
      const target = value.target.trim();
      const allowed = allow.mutateAsync({
        target,
        duration: value.duration,
        note: value.note.trim() || undefined,
      });
      toast.promise(allowed, {
        loading: `Allowlisting ${target}…`,
        success: `Allowlisted ${target}`,
        error: (e: Error) => e.message || "Could not allowlist that",
      });
      await allowed.then(() => formApi.reset()).catch(() => undefined);
    },
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ListCheckIcon aria-hidden="true" className="size-4" />
          Never banned
        </CardTitle>
        <CardDescription>
          Addresses CrowdSec will not ban, and a ban already on one is lifted
          the moment it is added. An admin's address is added each time they log
          in and kept for 30 days.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <form
          className="flex flex-wrap items-start gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            e.stopPropagation();
            form.handleSubmit();
          }}
        >
          <form.Field
            name="target"
            validators={{
              onChange: ({ value }) =>
                value === "" || isBanTarget(value)
                  ? undefined
                  : "Enter an IP address or a CIDR range",
              onSubmit: ({ value }) =>
                isBanTarget(value)
                  ? undefined
                  : "Enter an IP address or a CIDR range",
            }}
            children={(field) => {
              const error = fieldError(field.state.meta.errors);
              return (
                <Field data-invalid={!!error} className="w-52">
                  <Input
                    aria-label="Address to allowlist"
                    placeholder="198.51.100.4 or a range"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                  />
                  {error && <FieldError>{error}</FieldError>}
                </Field>
              );
            }}
          />
          <form.Field
            name="duration"
            children={(field) => (
              <Select
                value={field.state.value}
                onValueChange={(v) => field.handleChange(v ?? "30d")}
              >
                <SelectTrigger aria-label="Allowlist for" className="w-36">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {ALLOW_DURATIONS.map((d) => (
                    <SelectItem key={d.value} value={d.value}>
                      {d.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          <form.Field
            name="note"
            children={(field) => (
              <Field className="w-56">
                <Input
                  aria-label="Note"
                  placeholder="Note (optional)"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(e) => field.handleChange(e.target.value)}
                />
              </Field>
            )}
          />
          <Button type="submit" variant="outline" disabled={allow.isPending}>
            Add
          </Button>
        </form>

        {isLoading ? (
          <Skeleton className="h-16 w-full" />
        ) : error ? (
          <p className="text-status-error text-sm">
            Could not read the allowlist: {(error as Error).message}
          </p>
        ) : items.length === 0 ? (
          <p className="text-muted-foreground text-sm">
            Nothing yet. Your address is added when you turn CrowdSec on and
            whenever an admin logs in.
          </p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Address</TableHead>
                <TableHead>Note</TableHead>
                <TableHead>Expires in</TableHead>
                <TableHead className="w-0" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {items.map((i) => (
                <AllowRow
                  key={i.value}
                  item={i}
                  yours={i.value === status.your_ip}
                />
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}

function AllowRow({
  item: i,
  yours,
}: {
  item: CrowdSecAllowItem;
  yours: boolean;
}) {
  const remove = useRemoveAllowed();
  return (
    <TableRow>
      <TableCell className="font-mono text-sm">
        {i.value}
        {yours && (
          <Badge variant="light" className="ml-2">
            you
          </Badge>
        )}
      </TableCell>
      <TableCell className="max-w-xs truncate" title={i.description}>
        {i.description}
      </TableCell>
      <TableCell className="text-muted-foreground whitespace-nowrap">
        {i.expires_at ? timeLeft(i.expires_at) : "never"}
      </TableCell>
      <TableCell>
        <Button
          variant="ghost"
          size="sm"
          aria-label={`Remove ${i.value} from the allowlist`}
          disabled={remove.isPending}
          onClick={() =>
            toast.promise(remove.mutateAsync(i.value), {
              loading: `Removing ${i.value}…`,
              success: `Removed ${i.value}`,
              error: (e: Error) => e.message || "Could not remove that",
            })
          }
        >
          Remove
        </Button>
      </TableCell>
    </TableRow>
  );
}

function AutobanCard({ status }: { status: CrowdSecStatus }) {
  const { policy, status: run, categories } = status.autoban;
  const { data: waf } = useWafStatus();
  const summary = describeAutoban(policy, run, waf ? waf.enabled : null);
  // Remounted when the stored policy changes, so the form always starts from it.
  return (
    <AutobanForm
      key={JSON.stringify(policy)}
      policy={policy}
      categories={categories}
      summary={summary}
      lastRun={run.last_run}
      active={run.active}
      crowdsecOn={status.enabled}
    />
  );
}

interface AutobanFormValues {
  enabled: boolean;
  threshold: number;
  window_minutes: number;
  duration_minutes: number;
  max_active: number;
  categories: string[];
}

function AutobanForm({
  policy,
  categories,
  summary,
  lastRun,
  active,
  crowdsecOn,
}: {
  policy: CrowdSecPolicy;
  categories: string[];
  summary: ReturnType<typeof describeAutoban>;
  lastRun: string | undefined;
  active: number;
  crowdsecOn: boolean;
}) {
  const save = useSetCrowdSecAutoban();
  const [pending, setPending] = useState<CrowdSecPolicy | null>(null);

  const persist = (p: CrowdSecPolicy) =>
    toast.promise(save.mutateAsync(p), {
      loading: "Saving…",
      success: p.enabled ? "Automatic bans are on" : "Automatic bans are off",
      error: (e: Error) => e.message || "Could not save the policy",
    });

  const form = useForm({
    defaultValues: {
      enabled: policy.enabled,
      threshold: policy.threshold,
      window_minutes: policy.window_minutes,
      duration_minutes: policy.duration_minutes,
      max_active: policy.max_active,
      categories: policy.categories,
    } as AutobanFormValues,
    onSubmit: ({ value }) => {
      // Turning WAF detections into bans is the one step on this page that can
      // hit a real visitor, so it is confirmed. Saving anything else is not.
      if (value.enabled && !policy.enabled) {
        setPending(value);
        return;
      }
      persist(value);
    },
  });

  const intField = (
    name: "threshold" | "window_minutes" | "duration_minutes" | "max_active",
    label: string,
    min: number,
    max: number,
    unit: string,
  ) => (
    <form.Field
      name={name}
      validators={{
        onChange: ({ value }) =>
          Number.isInteger(value) && value >= min && value <= max
            ? undefined
            : `Between ${min} and ${max}`,
      }}
      children={(field) => {
        const error = fieldError(field.state.meta.errors);
        return (
          <Field data-invalid={!!error} className="w-40">
            <Label htmlFor={`autoban-${name}`}>{label}</Label>
            <Input
              id={`autoban-${name}`}
              type="number"
              inputMode="numeric"
              min={min}
              max={max}
              value={Number.isNaN(field.state.value) ? "" : field.state.value}
              onBlur={field.handleBlur}
              onChange={(e) => field.handleChange(e.target.valueAsNumber)}
            />
            <span className="text-text-faint text-xs">{unit}</span>
            {error && <FieldError>{error}</FieldError>}
          </Field>
        );
      }}
    />
  );

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <BrickWallFireIcon aria-hidden="true" className="size-4" />
          Automatic bans
        </CardTitle>
        <CardDescription>
          Ban an address that keeps tripping the WAF's attack rules. Off until
          you turn it on.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            e.stopPropagation();
            form.handleSubmit();
          }}
        >
          <form.Field
            name="enabled"
            children={(field) => (
              <div className="flex items-center gap-2">
                <Checkbox
                  id="autoban-enabled"
                  checked={field.state.value}
                  onCheckedChange={(c) => field.handleChange(c === true)}
                />
                <Label htmlFor="autoban-enabled">
                  Ban automatically from WAF detections
                </Label>
              </div>
            )}
          />
          <div className="flex flex-wrap gap-4">
            {intField("threshold", "Requests", 3, 1000, "separate requests")}
            {intField("window_minutes", "Within", 1, 1440, "minutes")}
            {intField("duration_minutes", "Ban for", 5, 10080, "minutes")}
            {intField("max_active", "At most", 1, 500, "bans at once")}
          </div>
          <form.Field
            name="categories"
            validators={{
              onChange: ({ value, fieldApi }) =>
                fieldApi.form.state.values.enabled && value.length === 0
                  ? "Pick at least one category"
                  : undefined,
            }}
            children={(field) => {
              const error = fieldError(field.state.meta.errors);
              return (
                <Field data-invalid={!!error}>
                  <Label>Which attacks count</Label>
                  <div className="flex flex-wrap gap-x-5 gap-y-2">
                    {categories.map((c) => {
                      const id = `autoban-cat-${c}`;
                      return (
                        <div key={c} className="flex items-center gap-2">
                          <Checkbox
                            id={id}
                            checked={field.state.value.includes(c)}
                            onCheckedChange={(on) =>
                              field.handleChange(
                                on === true
                                  ? [...field.state.value, c]
                                  : field.state.value.filter((x) => x !== c),
                              )
                            }
                          />
                          <Label htmlFor={id} className="font-mono text-xs">
                            {c.replace(/^attack-/, "")}
                          </Label>
                        </div>
                      );
                    })}
                  </div>
                  {error && <FieldError>{error}</FieldError>}
                </Field>
              );
            }}
          />
          <form.Subscribe
            selector={(s) => [s.canSubmit, s.isDirty] as const}
            children={([canSubmit, isDirty]) => (
              <Button
                type="submit"
                variant="outline"
                disabled={!canSubmit || !isDirty || save.isPending}
              >
                Save
              </Button>
            )}
          />
        </form>

        <div className="space-y-1.5 text-sm">
          <p className="text-muted-foreground">{summary.rule}</p>
          {summary.warnings.map((w) => (
            <p key={w.text} className={cn(TEXT_TONE[w.tone])}>
              {w.text}
            </p>
          ))}
          {policy.enabled && !crowdsecOn && (
            <p className={TEXT_TONE.attention}>
              CrowdSec itself is off, so nothing is banned.
            </p>
          )}
          {policy.enabled && crowdsecOn && (
            <p className="text-text-faint text-xs">
              {active} automatic {active === 1 ? "ban" : "bans"} in force
              {lastRun ? "." : "; not checked yet."}
            </p>
          )}
        </div>
      </CardContent>

      <AlertDialog
        open={pending !== null}
        onOpenChange={() => setPending(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Ban automatically?</AlertDialogTitle>
            <AlertDialogDescription>
              Before you do, what this does and does not do:
            </AlertDialogDescription>
          </AlertDialogHeader>
          <ul className="text-muted-foreground list-disc space-y-1.5 pl-5 text-sm">
            {pending &&
              autobanEnablePoints(pending).map((p) => <li key={p}>{p}</li>)}
          </ul>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (pending) persist(pending);
              }}
            >
              Turn on
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}

const LIFT_ALL =
  "cd /opt/belune\ndocker compose exec crowdsec cscli decisions delete --all";
const LIFT_ONE =
  "docker compose exec crowdsec cscli decisions delete --ip 203.0.113.9";
const TURN_OFF =
  'docker compose exec postgres psql -U belune -d belune -c "UPDATE crowdsec_config SET enabled = false"';

/**
 * The way back when the dashboard itself cannot be reached. Written for the
 * moment it is needed, so it names the commands and says what will not work.
 */
function LockoutCard() {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ShieldXIcon aria-hidden="true" className="size-4" />
          If a ban locks you out
        </CardTitle>
        <CardDescription>
          This dashboard is never behind the bouncer, so you can lift any ban
          from the table above. Only read on if you cannot reach the dashboard
          at all.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        <p className="text-muted-foreground">
          That can happen if a proxy or CDN in front of Belune shares one
          address with every visitor and that address got banned. Connect to the
          server over SSH. The host shell in this dashboard will not help: it
          sits behind the same login you cannot reach.
        </p>
        <div className="space-y-1.5">
          <p className="font-medium">Lift every ban</p>
          <Command text={LIFT_ALL} />
          <p className="text-muted-foreground">
            Or just one address, taking effect within a few seconds:
          </p>
          <Command text={LIFT_ONE} />
        </div>
        <div className="space-y-1.5">
          <p className="font-medium">
            Switch CrowdSec off without the dashboard
          </p>
          <Command text={TURN_OFF} />
          <p className="text-muted-foreground">
            Belune removes the bouncer from Caddy within 30 seconds.
          </p>
        </div>
      </CardContent>
    </Card>
  );
}

function Command({ text }: { text: string }) {
  return (
    <pre className="bg-muted overflow-x-auto rounded-md p-3 font-mono text-xs">
      {text}
    </pre>
  );
}
