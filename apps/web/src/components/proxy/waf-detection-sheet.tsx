import { useState, type ReactNode } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  BADGE_VARIANT,
  categoryLabel,
  describeEventVerdict,
  DETECTION_HEADERS_ALLOWLIST,
  DETECTION_PAYLOAD_ABSENT,
  detectionHeaders,
  eventHost,
  MATCHED_TARGET_ABSENT,
  MATCHED_TARGET_NOTE,
  REQUEST_DETAIL_ABSENT,
  statusTone,
} from "@/components/proxy/waf-view";
import { WafExclusionDialog } from "@/components/proxy/waf-exclusion-dialog";
import { formatDateTime, formatRelativeTime } from "@/lib/utils/format";
import type { WafEvent } from "@/lib/types";

/**
 * Everything Belune kept about one detection.
 *
 * A panel rather than more columns because the URL is the long field and the
 * one that matters: a query string carrying an injection attempt runs past any
 * column width, and truncating it is exactly the part the operator needs to
 * read to judge the detection. The table stays scannable; this is where a row
 * is judged.
 */
export function WafDetectionSheet({
  event,
  open,
  onOpenChange,
}: {
  event: WafEvent;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const category = categoryLabel(event.category);
  const verdict = describeEventVerdict(event);
  const headers = detectionHeaders(event.request);
  const status =
    event.request && event.request.status >= 100 && event.request.status <= 599
      ? event.request.status
      : null;
  const relative = formatRelativeTime(event.recorded_at);
  const [excluding, setExcluding] = useState(false);

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="sm:max-w-lg">
        <SheetHeader>
          {/* The rule's message is the human reading of the detection; the id is
              what an operator searches the CRS for, so both are here. */}
          <SheetTitle className="pr-8">
            {event.message || `Rule ${event.rule_id}`}
          </SheetTitle>
          <SheetDescription>
            Rule {event.rule_id}
            {category && ` · ${category}`}
          </SheetDescription>
        </SheetHeader>

        {/* One badge. For every rule but the anomaly summary the verdict label
            IS the severity, so a severity badge beside it said the same word
            twice; for that one rule the verdict is the truer of the two, since
            Coraza reports its severity as "unknown". */}
        <div>
          <Badge
            variant={
              verdict.strong ? "destructive-solid" : BADGE_VARIANT[verdict.tone]
            }
            title={verdict.title ?? undefined}
          >
            {verdict.label}
          </Badge>
        </div>

        <dl className="flex flex-col gap-3">
          <DetailRow label="Date">
            <span className="font-mono text-xs tabular-nums">
              {formatDateTime(event.recorded_at)}
            </span>
            <span className="text-muted-foreground ml-2 text-xs">
              {relative === "now" ? "just now" : `${relative} ago`}
            </span>
          </DetailRow>
          <DetailRow label="Client">
            <span className="font-mono text-xs">{event.client_ip}</span>
          </DetailRow>
          <DetailRow label="Host">
            <span className="font-mono text-xs">{eventHost(event)}</span>
          </DetailRow>
          {/* Directly under the identity of the detection, above the request:
              it is the field that answers "is this mine or theirs", which is
              the question the panel exists for. */}
          <DetailRow label="Matched in">
            {event.matched_target ? (
              <span className="font-mono text-xs break-all">
                {event.matched_target}
              </span>
            ) : (
              <span
                className="text-muted-foreground cursor-help text-xs italic"
                title={MATCHED_TARGET_ABSENT}
              >
                no single field
              </span>
            )}
          </DetailRow>
          {event.request ? (
            <>
              <DetailRow label="Request">
                <span className="font-mono text-xs">
                  {event.request.method}
                </span>
                {status !== null && (
                  <Badge
                    variant={BADGE_VARIANT[statusTone(status)]}
                    className="ml-2"
                  >
                    {status}
                  </Badge>
                )}
              </DetailRow>
              <DetailRow label="URL">
                {/* break-all, not truncate: the query string is the evidence,
                    and this panel exists because the table had to cut it. */}
                <span className="font-mono text-xs break-all">
                  {event.request.path}
                </span>
              </DetailRow>
            </>
          ) : (
            <DetailRow label="URL">
              <span className="text-muted-foreground text-xs italic">
                {REQUEST_DETAIL_ABSENT}
              </span>
            </DetailRow>
          )}
        </dl>

        <Separator />

        <section className="flex flex-col gap-2">
          <h3 className="text-sm font-medium">Headers</h3>
          <p className="text-muted-foreground text-xs">
            {DETECTION_HEADERS_ALLOWLIST}
          </p>
          {headers.length > 0 ? (
            <dl className="flex flex-col gap-2">
              {headers.map((h) => (
                <DetailRow key={h.name} label={h.name}>
                  <span className="font-mono text-xs break-all">{h.value}</span>
                </DetailRow>
              ))}
            </dl>
          ) : (
            <p className="text-muted-foreground text-xs italic">
              {/* Two reasons, and they are different: a GET simply has no
                  content type, while a missing request log means the headers
                  were swept with it. */}
              {event.request
                ? "Neither header was present on this request."
                : "They went with the request log."}
            </p>
          )}
        </section>

        {/* Flowing under the headers, not pinned to the bottom of the panel:
            most detections fill a third of it, and a note floating 500px below
            the last line reads as page furniture rather than as part of this
            record. Seen in a browser. */}
        <p className="text-muted-foreground text-xs">
          {event.matched_target
            ? `${MATCHED_TARGET_NOTE} ${DETECTION_PAYLOAD_ABSENT}`
            : DETECTION_PAYLOAD_ABSENT}
        </p>

        <Separator />

        {/* A false positive is judged here, with the detection in front of the
            operator, so this is where the exclusion starts. It opens a dialog that
            says what it lowers; it never excludes anything by itself. */}
        <section className="flex flex-col items-start gap-2">
          <h3 className="text-sm font-medium">
            Think this is a false positive?
          </h3>
          <p className="text-muted-foreground text-xs">
            An exclusion tells the WAF to stop recording this rule for what you
            choose. It lowers your protection, so it asks you to confirm.
          </p>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => setExcluding(true)}
          >
            Create Exclusion
          </Button>
        </section>
        {excluding && (
          <WafExclusionDialog
            event={event}
            open
            onOpenChange={(open) => !open && setExcluding(false)}
          />
        )}
      </SheetContent>
    </Sheet>
  );
}

function DetailRow({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <div className="grid grid-cols-[6rem_1fr] items-baseline gap-x-3">
      <dt className="text-muted-foreground text-xs">{label}</dt>
      <dd className="min-w-0">{children}</dd>
    </div>
  );
}
