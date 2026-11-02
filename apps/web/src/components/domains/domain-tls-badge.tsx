import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { unroutedNotice } from "@/lib/domain-tls";
import { useRecheckDomainTLS } from "@/lib/hooks/use-certificates";
import type { DomainExpanded, TLSStatus } from "@/lib/types";
import { RefreshCw } from "lucide-react";

// Mirrors TLS_STATUS_STYLES on the certificates page — the two views must not
// disagree about what a healthy certificate looks like. Coloured text on a
// low-opacity fill of the same colour with a matching border, which is the shape
// `destructive` already had. Active is the accent (`light`), so it follows the
// user's accent; Pending/Expiring use the amber status tokens.
const STYLES: Record<
  TLSStatus,
  {
    label: string;
    variant: "default" | "secondary" | "outline" | "destructive" | "light";
    className?: string;
  }
> = {
  active: { label: "Active", variant: "light" },
  pending: {
    label: "Pending",
    variant: "outline",
    className:
      "border-status-building-line bg-status-building-soft text-status-building",
  },
  expiring: {
    label: "Expiring",
    variant: "outline",
    className:
      "border-status-building-line bg-status-building-soft text-status-building",
  },
  expired: { label: "Expired", variant: "destructive" },
  failed: { label: "Failed", variant: "destructive" },
  disabled: { label: "Off", variant: "outline" },
  // The proxy is configured to issue from its own CA (dev). A real, finished
  // state — not a step on the way to a public certificate — but the certificate
  // is trusted by nothing, so it is not dressed up as Active.
  local: { label: "Local", variant: "secondary" },
  unknown: { label: "Checking…", variant: "secondary" },
};

function formatDate(iso?: string | null): string | null {
  if (!iso) return null;
  return new Date(iso).toLocaleString();
}

/**
 * The badge reports what the server last observed on the wire — the certificate
 * a browser would actually be handed — rather than inferring a status from the
 * domain's configuration. A configured domain whose certificate never issued
 * used to look identical to a working one; now it says so, and says why.
 */
export function DomainTLSBadge({
  domain,
  projectId,
  applicationId,
  certificateName,
  applicationStatus,
}: {
  domain: DomainExpanded;
  projectId: string;
  applicationId: string;
  /** Resolved by the caller from /domains/tls — the domain row itself carries
   *  only certificate_id, which names nothing on its own. Undefined whenever
   *  the domain is not serving an uploaded certificate. */
  certificateName?: string;
  /** The application's status, to explain a domain that is pending because the
   *  application is not running. */
  applicationStatus?: string;
}) {
  const recheck = useRecheckDomainTLS(projectId, applicationId);
  const status: TLSStatus = domain.tls_status ?? "unknown";
  const style = STYLES[status] ?? STYLES.unknown;

  const unrouted = unroutedNotice(domain.ssl_mode, applicationStatus);
  const expires = formatDate(domain.tls_not_after);
  const checked = formatDate(domain.tls_last_checked_at);

  return (
    <Popover>
      <PopoverTrigger
        render={
          <button type="button" aria-label={`TLS status: ${style.label}`} />
        }
      >
        {/* No "TLS:" prefix — the badge's only home is a column already headed
            TLS, and repeating it in every cell is noise. */}
        <Badge
          variant={style.variant}
          className={`${style.className ?? ""} cursor-pointer`}
        >
          {style.label}
        </Badge>
      </PopoverTrigger>
      <PopoverContent className="w-80 space-y-3 text-sm">
        <div className="flex items-center justify-between gap-2">
          <span className="font-medium">{domain.hostname}</span>
          <Badge variant={style.variant} className={style.className}>
            {style.label}
          </Badge>
        </div>

        {domain.tls_error && (
          <p className="text-destructive bg-destructive/10 rounded-md p-2 text-xs break-words">
            {domain.tls_error}
          </p>
        )}

        {/* An advisory is a suspicion, not a verdict — styled as a caution rather
            than an error, because a hostname resolving somewhere that isn't us is
            also just what a proxy in front of us looks like. */}
        {/* Said before the advisory and the generic wait: with no route nothing
            is waiting on a certificate, and a hostname-resolution suspicion
            would be about a request that cannot arrive. */}
        {!domain.tls_error && unrouted && (
          <p className="text-status-building bg-status-building/10 rounded-md p-2 text-xs break-words">
            {unrouted}
          </p>
        )}

        {!domain.tls_error && !unrouted && domain.tls_advisory && (
          <p className="text-status-building bg-status-building/10 rounded-md p-2 text-xs break-words">
            {domain.tls_advisory}
          </p>
        )}

        <dl className="text-muted-foreground grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
          {certificateName && (
            <>
              <dt>Certificate</dt>
              <dd className="text-foreground break-words">{certificateName}</dd>
            </>
          )}
          {domain.tls_issuer && (
            <>
              <dt>Issuer</dt>
              <dd className="text-foreground break-words">
                {domain.tls_issuer}
              </dd>
            </>
          )}
          {expires && (
            <>
              <dt>Expires</dt>
              <dd className="text-foreground">{expires}</dd>
            </>
          )}
          <dt>Checked</dt>
          <dd className="text-foreground">{checked ?? "not yet"}</dd>
        </dl>

        {status === "pending" &&
          !unrouted &&
          !domain.tls_error &&
          !domain.tls_advisory && (
            <p className="text-muted-foreground text-xs">
              Waiting on a certificate. Issuance needs ports 80 and 443
              reachable from the internet and this hostname pointing at this
              server.
            </p>
          )}

        <Button
          size="sm"
          variant="outline"
          className="w-full"
          disabled={recheck.isPending}
          onClick={() => recheck.mutate(domain.id)}
        >
          <RefreshCw className="mr-2 h-3.5 w-3.5" />
          {recheck.isPending ? "Rechecking…" : "Recheck now"}
        </Button>
      </PopoverContent>
    </Popover>
  );
}
