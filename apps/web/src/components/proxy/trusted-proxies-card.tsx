import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { ArrowUpRightIcon, NetworkIcon, TriangleAlertIcon } from "lucide-react";
import { toast } from "sonner";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Field, FieldError } from "@/components/ui/field";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import {
  addRanges,
  describeTrusted,
  formatRanges,
  parseRanges,
  sameRanges,
  validateRanges,
} from "@/components/proxy/trusted-proxies-view";
import {
  useSetTrustedProxies,
  useTrustedProxies,
} from "@/lib/hooks/use-trusted-proxies";
import { fieldError } from "@/lib/utils/field-error";
import type { TrustedProxies, TrustedProxyOverlap } from "@/lib/types";

/**
 * The CDN or load balancer in front of Caddy, if there is one.
 *
 * This list is a trust boundary and not a convenience: Caddy believes what a listed
 * peer says about who the visitor is, and WAF detections, request logs and bans all
 * use that address. So the warning is always on screen, not behind a hover, and an
 * overlap with a Docker network Caddy is on is shown where the list is edited.
 */
export function TrustedProxiesCard() {
  const { data, isLoading, error } = useTrustedProxies();

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <NetworkIcon aria-hidden="true" className="size-4" />
          Trusted Proxies
        </CardTitle>
        <CardDescription>
          A CDN or load balancer in front of Caddy. Leave this empty if visitors
          connect straight to this server. Listing one makes Caddy read the real
          visitor from the address the proxy reports, so detections and bans
          land on them and not on the proxy.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <Alert variant="warning">
          <TriangleAlertIcon aria-hidden="true" />
          <AlertTitle>
            Every address listed here can claim to be anyone
          </AlertTitle>
          <AlertDescription>
            Whatever a listed proxy says the visitor&apos;s address is, Belune
            believes: in WAF detections, in request logs and in CrowdSec bans.
            List only a CDN you use or a network you control, and never a range
            your own apps run in.
          </AlertDescription>
        </Alert>

        {isLoading ? (
          <Skeleton className="h-32 w-full" />
        ) : error || !data ? (
          <p className="text-status-error text-sm">
            Could not read the trusted proxies:{" "}
            {(error as Error | null)?.message ?? "no answer"}
          </p>
        ) : (
          // Keyed on the stored list so the box follows the server after a save
          // (the canonical, sorted form) instead of keeping what was typed.
          <TrustedProxiesForm key={data.cidrs.join(",")} data={data} />
        )}
      </CardContent>
    </Card>
  );
}

function TrustedProxiesForm({ data }: { data: TrustedProxies }) {
  const save = useSetTrustedProxies();
  const [added, setAdded] = useState<string | null>(null);

  const form = useForm({
    defaultValues: { ranges: formatRanges(data.cidrs) },
    onSubmit: async ({ value }) => {
      const saved = save.mutateAsync(parseRanges(value.ranges));
      toast.promise(saved, {
        loading: "Saving the trusted proxies…",
        success: (s) =>
          s.cidrs.length === 0
            ? "Stopped trusting any proxy"
            : `Trusting ${s.cidrs.length} ${s.cidrs.length === 1 ? "range" : "ranges"}`,
        error: (e: Error) => e.message || "Could not save the trusted proxies",
      });
      await saved.catch(() => undefined);
    },
  });

  const preset = data.presets[0];

  return (
    <form
      className="space-y-4"
      onSubmit={(e) => {
        e.preventDefault();
        e.stopPropagation();
        form.handleSubmit();
      }}
    >
      <p className="text-muted-foreground text-sm">
        {describeTrusted(data.cidrs.length)}
      </p>

      <Overlaps overlaps={data.overlaps} checked={data.overlaps_checked} />

      <form.Field
        name="ranges"
        validators={{
          onChange: ({ value }) => validateRanges(value),
          onSubmit: ({ value }) => validateRanges(value),
        }}
        children={(field) => {
          // Not isTouched, which flips on the first keystroke: an error should wait
          // for the operator to finish, or for a submit attempt.
          const show =
            field.state.meta.isBlurred || form.state.submissionAttempts > 0;
          const message = show
            ? fieldError(field.state.meta.errors)
            : undefined;
          return (
            <Field data-invalid={!!message}>
              <Label htmlFor="trusted-proxy-ranges">
                Addresses or ranges, one per line
              </Label>
              <Textarea
                id="trusted-proxy-ranges"
                className="min-h-28 font-mono text-xs"
                spellCheck={false}
                autoComplete="off"
                placeholder={"203.0.113.0/24\n2001:db8::/32"}
                value={field.state.value}
                aria-invalid={!!message}
                onBlur={field.handleBlur}
                onChange={(e) => {
                  setAdded(null);
                  field.handleChange(e.target.value);
                }}
              />
              {message && <FieldError>{message}</FieldError>}
            </Field>
          );
        }}
      />

      {preset && (
        <div className="space-y-1">
          <form.Subscribe
            selector={(s) => s.values.ranges}
            children={(ranges) => (
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => {
                  form.setFieldValue("ranges", addRanges(ranges, preset.cidrs));
                  setAdded(preset.name);
                }}
              >
                Add {preset.name} Ranges
              </Button>
            )}
          />
          <p className="text-muted-foreground text-xs">
            A snapshot copied on {preset.checked_at} from{" "}
            <a
              href={preset.source}
              target="_blank"
              rel="noreferrer noopener"
              className="text-brand-text inline-flex items-center gap-0.5 hover:underline"
            >
              {new URL(preset.source).host}
              <ArrowUpRightIcon aria-hidden="true" className="size-3" />
            </a>
            . Belune does not keep it current: check the list there before you
            rely on it.
            {added && ` Added the ${added} ranges below. Save to apply them.`}
          </p>
        </div>
      )}

      <form.Subscribe
        selector={(s) => s.values.ranges}
        children={(ranges) => {
          const dirty = !sameRanges(parseRanges(ranges), data.cidrs);
          return (
            <div className="flex items-center gap-3">
              <Button type="submit" disabled={!dirty || save.isPending}>
                Save
              </Button>
              <p className="text-muted-foreground text-xs">
                Applied to Caddy within seconds of saving. Saving an empty list
                stops trusting any proxy.
              </p>
            </div>
          );
        }}
      />
    </form>
  );
}

function Overlaps({
  overlaps,
  checked,
}: {
  overlaps: TrustedProxyOverlap[];
  checked: boolean;
}) {
  if (!checked) {
    return (
      <p className="text-muted-foreground text-xs">
        Belune could not list its Docker networks just now, so it could not
        check whether a range here overlaps one Caddy is on.
      </p>
    );
  }
  if (overlaps.length === 0) return null;
  return (
    <Alert variant="destructive">
      <TriangleAlertIcon aria-hidden="true" />
      <AlertTitle>
        {overlaps.length === 1
          ? "A trusted range covers a network your apps run on"
          : "Trusted ranges cover networks your apps run on"}
      </AlertTitle>
      <AlertDescription>
        <p>
          Caddy is attached to these Docker networks, so any container on them
          can connect to it. Because their addresses are trusted, such a
          container can claim to be any visitor, and with automatic bans on,
          have that address banned. Narrow the range so it does not include
          them.
        </p>
        <ul className="mt-2 space-y-0.5 font-mono text-xs">
          {overlaps.map((o) => (
            <li key={`${o.cidr}|${o.network}|${o.subnet}`}>
              {o.cidr} covers {o.network} ({o.subnet})
            </li>
          ))}
        </ul>
      </AlertDescription>
    </Alert>
  );
}
