import { ListXIcon, Trash2Icon } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { IconAction } from "@/components/ui/icon-action";
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
  describeIgnored,
  scopeBadge,
} from "@/components/proxy/waf-exclusion-view";
import { useDeleteWafExclusion, useWafExclusions } from "@/lib/hooks/use-waf";
import { formatRelativeTime } from "@/lib/utils/format";
import type { WafExclusion } from "@/lib/types";

/** The id the WAF card's "in effect" line scrolls to. */
export const WAF_EXCLUSIONS_ANCHOR = "waf-exclusions";

/**
 * Every place the rule set has been told to stand down.
 *
 * An exclusion's whole effect is that detections stop appearing, so this list is
 * the visible reason for that silence. The two that widen what goes unwatched, an
 * exclusion for every application and one that turns a rule off, are marked in
 * colour as well as in words, and the reason given for turning a rule off is shown
 * in full: it is the part that has to outlive whoever typed it.
 */
export function WafExclusionsCard() {
  const { data, isLoading, error } = useWafExclusions();
  const remove = useDeleteWafExclusion();

  const drop = (e: WafExclusion) =>
    toast.promise(remove.mutateAsync(e.id), {
      loading: "Removing the exclusion…",
      success: `Rule ${e.rule_id} is back in force`,
      error: (err: Error) => err.message || "Could not remove the exclusion",
    });

  return (
    <Card id={WAF_EXCLUSIONS_ANCHOR}>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ListXIcon aria-hidden="true" className="size-4" />
          Exclusions
          {data && (
            <span className="text-muted-foreground text-xs font-normal tabular-nums">
              {data.exclusions.length} of {data.max}
            </span>
          )}
        </CardTitle>
        <CardDescription>
          Places the rule set has been told to stand down. Detections they cover
          are not recorded, so a quiet rule here is not a clean application.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <Skeleton className="h-16 w-full" />
        ) : error || !data ? (
          <p className="text-status-error text-sm">
            Could not read the exclusions:{" "}
            {(error as Error | null)?.message ?? "no answer"}
          </p>
        ) : data.exclusions.length === 0 ? (
          <p className="text-muted-foreground text-sm">
            None. The WAF reports everything the rule set matches. To add one,
            open a detection and choose Create Exclusion.
          </p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Rule</TableHead>
                <TableHead>Ignores</TableHead>
                <TableHead>Where</TableHead>
                <TableHead>Why</TableHead>
                <TableHead>Added</TableHead>
                <TableHead className="w-0" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.exclusions.map((e) => (
                <Row key={e.id} e={e} onRemove={() => drop(e)} />
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}

function Row({ e, onRemove }: { e: WafExclusion; onRemove: () => void }) {
  const where = scopeBadge(e);
  const wholeRule = e.shape === "rule";
  return (
    <TableRow>
      <TableCell className="font-mono text-xs tabular-nums">
        {e.rule_id}
      </TableCell>
      <TableCell
        className={
          wholeRule
            ? "text-status-building text-xs font-medium"
            : "font-mono text-xs break-all"
        }
      >
        {describeIgnored(e)}
      </TableCell>
      <TableCell>
        <Badge
          variant={
            where.tone === "warning"
              ? "destructive"
              : where.tone === "muted"
                ? "ghost"
                : "outline"
          }
          title={
            e.inert
              ? "This application has no domains, so the exclusion applies to nothing right now."
              : e.hosts.length > 0
                ? e.hosts.join(", ")
                : undefined
          }
        >
          {where.label}
        </Badge>
      </TableCell>
      <TableCell
        className={
          wholeRule
            ? "text-status-building max-w-64 text-xs break-words whitespace-normal"
            : "text-muted-foreground max-w-64 text-xs break-words whitespace-normal"
        }
      >
        {e.note || "No reason given"}
      </TableCell>
      <TableCell className="text-muted-foreground text-xs whitespace-nowrap">
        {formatRelativeTime(e.created_at)}
      </TableCell>
      <TableCell>
        <IconAction label="Remove exclusion" destructive onClick={onRemove}>
          <Trash2Icon aria-hidden="true" />
        </IconAction>
      </TableCell>
    </TableRow>
  );
}
