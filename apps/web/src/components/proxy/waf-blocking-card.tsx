import { useState } from "react";
import { ShieldAlertIcon } from "lucide-react";
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
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
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
  BLOCKING_CONFIRM_POINTS,
  describeBlockingImpact,
} from "@/components/proxy/waf-blocking-view";
import { useSetWafBlocking, useWafBlocking } from "@/lib/hooks/use-waf";
import type { WafBlockingApplication } from "@/lib/types";

/**
 * Which applications the WAF refuses requests for. Off by default and per
 * application: one noisy application must not decide for the rest, and there is
 * deliberately no switch for every application at once.
 */
export function WafBlockingCard() {
  const { data, isLoading, error } = useWafBlocking();
  const set = useSetWafBlocking();
  const [pending, setPending] = useState<WafBlockingApplication | null>(null);

  const apply = (app: WafBlockingApplication, enabled: boolean) =>
    toast.promise(
      set.mutateAsync({ applicationId: app.application_id, enabled }),
      {
        loading: enabled ? "Turning blocking on…" : "Turning blocking off…",
        success: enabled
          ? `${app.application_name} now refuses requests the rule set flags`
          : `${app.application_name} is back to detection only`,
        error: (err: Error) => err.message || "Could not change the WAF mode",
      },
    );

  const onToggle = (app: WafBlockingApplication, next: boolean) => {
    // Turning it OFF is never gated: reversing it must always be one click.
    if (next) setPending(app);
    else apply(app, false);
  };

  const impact = pending ? describeBlockingImpact(pending) : null;

  return (
    <Card id="waf-blocking">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ShieldAlertIcon aria-hidden="true" className="size-4" />
          Blocking
        </CardTitle>
        <CardDescription>
          By default the WAF only records. Turn blocking on for an application
          to refuse the requests the rule set scores over its threshold.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <Skeleton className="h-16 w-full" />
        ) : error || !data ? (
          <p className="text-status-error text-sm">
            Could not read the applications:{" "}
            {(error as Error | null)?.message ?? "no answer"}
          </p>
        ) : data.applications.length === 0 ? (
          <p className="text-muted-foreground text-sm">
            No application has a domain yet, so there is nothing for the WAF to
            enforce on.
          </p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Application</TableHead>
                <TableHead>Hostnames</TableHead>
                <TableHead className="w-0">Blocking</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.applications.map((app) => (
                <TableRow key={app.application_id}>
                  <TableCell>
                    <div className="text-sm font-medium">
                      {app.application_name}
                    </div>
                    <div className="text-muted-foreground text-xs">
                      {app.project_name}
                    </div>
                  </TableCell>
                  <TableCell className="font-mono text-xs break-all">
                    {app.hosts.join(", ")}
                    {app.blocking && app.shared_hosts.length > 0 && (
                      <Badge
                        variant="outline"
                        className="ml-2"
                        title={`${app.shared_hosts.join(", ")} also served by an application that has not opted in, so it only detects.`}
                      >
                        Partly detection only
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell>
                    <Switch
                      checked={app.blocking}
                      onCheckedChange={(v) => onToggle(app, v)}
                      aria-label={`Block requests for ${app.application_name}`}
                    />
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>

      <AlertDialog
        open={pending !== null}
        onOpenChange={(open) => !open && setPending(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Block requests for {pending?.application_name}?
            </AlertDialogTitle>
            <AlertDialogDescription>
              {impact && impact.enforced.length > 0
                ? "These hostnames will start refusing requests:"
                : "None of this application's hostnames can be enforced on yet."}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {impact && impact.enforced.length > 0 && (
            <ul className="font-mono text-xs">
              {impact.enforced.map((h) => (
                <li key={h}>{h}</li>
              ))}
            </ul>
          )}
          {impact && impact.detectionOnly.length > 0 && (
            <p className="text-muted-foreground text-sm">
              Still detection only, because another application that has not
              opted in also serves {impact.detectionOnly.join(", ")}.
            </p>
          )}
          <ul className="text-muted-foreground list-disc space-y-1 pl-5 text-sm">
            {BLOCKING_CONFIRM_POINTS.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (pending) apply(pending, true);
                setPending(null);
              }}
            >
              Turn blocking on
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}
