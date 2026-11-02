import { HistoryIcon } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  describeHistory,
  summarizeCrowdSec,
  type CrowdSecTone,
} from "@/components/proxy/crowdsec-view";
import {
  useCrowdSecDecisions,
  useCrowdSecHistory,
} from "@/lib/hooks/use-crowdsec";
import { cn } from "@/lib/utils";
import { formatDateTime } from "@/lib/utils/format";
import type { CrowdSecStatus } from "@/lib/types";

const VALUE_TONE: Record<CrowdSecTone, string> = {
  neutral: "text-foreground",
  attention: "text-status-building",
  ready: "text-status-ready",
  error: "text-status-error",
};

/** CrowdSec at a glance, above the detail cards that each hold one part of it. */
export function SummaryCard({
  status,
  reachable,
}: {
  status: CrowdSecStatus;
  reachable: boolean;
}) {
  // Reading the bans runs a command in the CrowdSec container, so it is only
  // asked when that is answering; the tile says "Unknown" otherwise.
  const { data } = useCrowdSecDecisions(reachable);
  const stats = summarizeCrowdSec(
    status,
    reachable ? data?.decisions : undefined,
    new Date(),
  );
  return (
    <Card>
      <CardContent>
        <dl className="grid gap-x-6 gap-y-4 sm:grid-cols-2 lg:grid-cols-4">
          {stats.map((s) => (
            <div key={s.key} className="min-w-0">
              <dt className="text-muted-foreground text-xs">{s.label}</dt>
              <dd className={cn("text-lg font-medium", VALUE_TONE[s.tone])}>
                {s.value}
              </dd>
              {s.hint && (
                <p className="text-muted-foreground mt-0.5 text-xs break-words">
                  {s.hint}
                </p>
              )}
            </div>
          ))}
        </dl>
      </CardContent>
    </Card>
  );
}

/**
 * Who banned what, and when. The automatic bans are the reason this exists: they
 * are written with no user, and the row says Belune did it.
 */
export function HistoryCard() {
  const { data, isLoading, error } = useCrowdSecHistory();
  const rows = (data?.items ?? []).map(describeHistory);
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <HistoryIcon aria-hidden="true" className="size-4" />
          History
        </CardTitle>
        <CardDescription>
          Bans placed and lifted, allowlist changes, and the bans Belune placed
          on its own. From the audit log, so it goes back as far as the log is
          kept.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <Skeleton className="h-16 w-full" />
        ) : error ? (
          <p className="text-status-error text-sm">
            Could not read the history: {(error as Error).message}
          </p>
        ) : rows.length === 0 ? (
          <p className="text-muted-foreground text-sm">Nothing yet.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>When</TableHead>
                <TableHead>By</TableHead>
                <TableHead>What</TableHead>
                <TableHead>Detail</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((e) => (
                <TableRow key={e.id}>
                  <TableCell className="text-muted-foreground whitespace-nowrap">
                    {formatDateTime(e.when)}
                  </TableCell>
                  <TableCell>
                    {e.actor.kind === "system" ? (
                      <Badge variant="secondary">{e.actor.label}</Badge>
                    ) : (
                      <span
                        className={cn(
                          e.actor.kind === "removed" && "text-muted-foreground",
                        )}
                      >
                        {e.actor.label}
                      </span>
                    )}
                  </TableCell>
                  <TableCell>
                    {e.what}
                    {e.target && (
                      <span className="ml-1.5 font-mono text-sm">
                        {e.target}
                      </span>
                    )}
                  </TableCell>
                  <TableCell
                    className="text-muted-foreground max-w-xs truncate"
                    title={e.detail ?? undefined}
                  >
                    {e.detail ?? "—"}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}
