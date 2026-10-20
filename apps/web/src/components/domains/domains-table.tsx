import { useMemo } from "react";
import { toast } from "sonner";
import type { ColumnDef } from "@tanstack/react-table";
import {
  CopyIcon,
  ExternalLinkIcon,
  MoreHorizontal,
  PencilIcon,
  SlidersHorizontalIcon,
  Trash2Icon,
} from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DataTable, buildActionColumnDef } from "@/components/ui/data-table";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { ExpiryCell } from "@/components/certificates/expiry-cell";
import { useRemoveDomain } from "@/lib/hooks/use-domains";
import type {
  DomainExpanded,
  NotAppliedFeature,
  RouteFeature,
} from "@/lib/types";
import { useProxyCapabilities } from "@/lib/hooks/use-proxy-capabilities";
import { featureStatus, inertExplanation } from "@/lib/route-feature-status";
import { useDomainTLSStatus } from "@/lib/hooks/use-certificates";
import { DomainTLSBadge } from "./domain-tls-badge";

const FEATURE_LABELS: Record<string, string> = {
  basic_auth: "Basic Auth",
  headers: "Custom Headers",
  ip_allowlist: "IP Allowlist",
  redirect: "Redirect",
  rate_limit: "Rate Limit",
};

/**
 * The domains of one application, as a table.
 *
 * A list read fine for one domain and badly for three. An app's domains are
 * siblings — admin / merchant / customer — and the questions you ask of them are
 * comparisons: which one has basic auth, which one is still pending a
 * certificate, did one drift onto a different port. In a list the badges sit at
 * whatever horizontal position the hostname's length leaves them, so the odd one
 * out is invisible. Columns are what make it obvious.
 *
 * Deliberately lean: no search, no pagination, no sorting. You do not search
 * three rows, and the chrome would cost more than it returns.
 */
export function DomainsTable({
  projectId,
  applicationId,
  domains,
  isLoading,
  onEdit,
  onEditFeatures,
  canDelete,
}: {
  projectId: string;
  applicationId: string;
  domains: DomainExpanded[];
  isLoading: boolean;
  onEdit: (domain: DomainExpanded) => void;
  onEditFeatures: (domain: DomainExpanded) => void;
  canDelete: boolean;
}) {
  // A domain row carries certificate_id but nothing to resolve it with:
  // ListDomainsByApplication is SELECT * FROM domains. /domains/tls is the only
  // read that joins certificates for the name, and since it became member-scoped
  // it is reachable here — so the operator who attached a certificate can see
  // WHICH one without holding the admin role. Gated on a custom certificate
  // actually being in use, so the common automatic-TLS case adds no request.
  const usesCustomCertificate = domains.some((d) => !!d.certificate_id);
  const { data: tlsRows } = useDomainTLSStatus(usesCustomCertificate);
  const certificateNameByDomain = useMemo(() => {
    const map = new Map<string, string>();
    for (const row of tlsRows ?? []) {
      if (row.certificate_name) map.set(row.id, row.certificate_name);
    }
    return map;
  }, [tlsRows]);

  const columns = useMemo<ColumnDef<DomainExpanded>[]>(
    () => [
      {
        accessorKey: "hostname",
        header: "Domain",
        cell: ({ row }) => <DomainLink domain={row.original} />,
      },
      {
        accessorKey: "container_port",
        header: "Port",
        cell: ({ row }) =>
          row.original.container_port ? (
            <span className="font-mono text-xs">
              {row.original.container_port}
            </span>
          ) : (
            <span className="text-muted-foreground">Default</span>
          ),
      },
      {
        accessorKey: "tls_status",
        header: "TLS",
        cell: ({ row }) => (
          <DomainTLSBadge
            domain={row.original}
            projectId={projectId}
            applicationId={applicationId}
            certificateName={certificateNameByDomain.get(row.original.id)}
          />
        ),
      },
      {
        accessorKey: "force_https",
        header: "HTTPS",
        cell: ({ row }) => <ForceHTTPSCell domain={row.original} />,
      },
      {
        accessorKey: "tls_not_after",
        header: "Expires",
        // Quiet for a local domain: Caddy's internal certificate lives 12 hours
        // and renews itself, so the usual "0d left" warning would be a standing
        // false alarm. Same component as the Certificates page, so the amber
        // "14d left" means the same thing in both places.
        cell: ({ row }) => (
          <ExpiryCell
            notAfter={row.original.tls_not_after}
            quiet={row.original.tls_status === "local"}
          />
        ),
      },
      {
        accessorKey: "route_features",
        header: "Features",
        // The names, not a count. "1 feature" tells you something is configured
        // but not what — and "what" is the entire question when you are checking
        // whether the admin subdomain is the one behind basic auth.
        cell: ({ row }) => (
          <FeatureCell
            features={row.original.route_features}
            notApplied={row.original.not_applied}
          />
        ),
      },
      buildActionColumnDef<DomainExpanded>({
        meta: { headerClassName: "text-right", className: "text-right" },
        cell: ({ row: { original: domain } }) => (
          <DomainActions
            projectId={projectId}
            applicationId={applicationId}
            domain={domain}
            onEdit={() => onEdit(domain)}
            onEditFeatures={() => onEditFeatures(domain)}
            canDelete={canDelete}
          />
        ),
      }),
    ],
    [
      projectId,
      applicationId,
      onEdit,
      onEditFeatures,
      canDelete,
      certificateNameByDomain,
    ],
  );

  return (
    <DataTable
      columns={columns}
      data={domains}
      isLoading={isLoading}
      getRowId={(d) => d.id}
      emptyMessage="No domains configured yet."
    />
  );
}

// The scheme follows the domain's own TLS mode rather than assuming https. An
// ssl_mode=off domain has no HTTPS listener for that name, so linking to https
// would hand the user a connection error on a domain that is working exactly as
// configured. (The header's "Open URL" button is blunter and always says https.)
function DomainLink({ domain }: { domain: DomainExpanded }) {
  const scheme = domain.ssl_mode === "off" ? "http" : "https";
  // The path is part of the address, so it belongs in the link. Linking to the
  // bare host on a /api domain would open the app that owns "/" — a different
  // application entirely — which looks like the domain pointing at the wrong
  // place.
  const path = domain.path && domain.path !== "/" ? domain.path : "";
  return (
    <a
      href={`${scheme}://${domain.hostname}${path}`}
      target="_blank"
      rel="noopener noreferrer"
      className="group inline-flex items-center gap-1.5 font-mono text-sm hover:underline"
    >
      <span>
        {domain.hostname}
        {path ? <span className="text-muted-foreground">{path}</span> : null}
      </span>
      <ExternalLinkIcon
        aria-hidden="true"
        className="text-muted-foreground size-3 shrink-0 opacity-0 transition-opacity group-hover:opacity-100"
      />
    </a>
  );
}

// How many feature badges a row shows before collapsing the rest into a count.
// Five features on one domain would otherwise wrap the cell and drag every
// other row's height with it.
const FEATURES_SHOWN = 2;

function featureLabel(f: RouteFeature): string {
  const label = FEATURE_LABELS[f.feature_type] ?? f.feature_type;
  return f.enabled ? label : `${label} · off`;
}

// Two settings decide this column, and only reading both gives the truth.
// force_https controls the redirect; ssl_mode controls whether there is an HTTPS
// listener for the name at all. Calling an ssl_mode=off domain "Optional" would
// promise HTTPS-if-you-want-it on a domain that cannot serve it, so that case
// says plainly that HTTP is all there is.
function ForceHTTPSCell({ domain }: { domain: DomainExpanded }) {
  if (domain.force_https) {
    return <Badge variant="light">Forced</Badge>;
  }
  return (
    <Badge variant="outline" className="text-muted-foreground">
      {domain.ssl_mode === "off" ? "HTTP only" : "Optional"}
    </Badge>
  );
}

function FeatureCell({
  features,
  notApplied,
}: {
  features?: RouteFeature[];
  notApplied?: NotAppliedFeature[];
}) {
  const { data: capabilities } = useProxyCapabilities();
  const list = features ?? [];
  if (list.length === 0) {
    return <span className="text-muted-foreground">—</span>;
  }

  const shown = list.slice(0, FEATURES_SHOWN);
  const hidden = list.slice(FEATURES_SHOWN);

  return (
    <div className="flex flex-wrap items-center gap-1">
      {shown.map((f) => {
        const { inert } = featureStatus(f, capabilities, notApplied);
        // Enabled in the database but not applied: the worst badge to get wrong,
        // since a "Rate Limit" badge on a domain with no rate limit is exactly
        // what an operator comparing siblings would trust. Dashed amber, with the
        // reason on hover, so it cannot pass for a live one.
        if (inert.length > 0) {
          return (
            <Badge
              key={f.id}
              className="border-dashed border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-400"
              title={inert.map(inertExplanation).join(" ")}
            >
              {featureLabel(f)} · not active
            </Badge>
          );
        }
        // A disabled feature is the dangerous case: basic auth that is present
        // but switched off looks, at a glance, exactly like basic auth. Say so.
        return f.enabled ? (
          <Badge key={f.id} variant="light">
            {featureLabel(f)}
          </Badge>
        ) : (
          <Badge key={f.id} variant="outline" className="text-muted-foreground">
            {featureLabel(f)}
          </Badge>
        );
      })}
      {hidden.length > 0 && (
        // Name the hidden ones on hover. A bare "+2 more" tells you that you are
        // being kept from something without telling you what, which is the worst
        // of both — one of those two could be a disabled basic auth.
        <span
          className="text-muted-foreground text-xs"
          title={hidden.map(featureLabel).join(", ")}
        >
          +{hidden.length} more
        </span>
      )}
    </div>
  );
}

function DomainActions({
  domain,
  projectId,
  applicationId,
  onEdit,
  onEditFeatures,
  canDelete,
}: {
  domain: DomainExpanded;
  projectId: string;
  applicationId: string;
  onEdit: () => void;
  onEditFeatures: () => void;
  canDelete: boolean;
}) {
  const removeDomain = useRemoveDomain(projectId, applicationId);

  const handleCopyHostname = () => {
    navigator.clipboard
      .writeText(domain.hostname)
      .then(() => toast.success("Hostname copied"))
      .catch(() => toast.error("Failed to copy"));
  };

  return (
    <div className="flex justify-end">
      <AlertDialog>
        <DropdownMenu>
          <DropdownMenuTrigger
            render={<Button size="icon" variant="ghost" aria-label="Actions" />}
          >
            <MoreHorizontal className="h-4 w-4" />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onClick={onEdit}>
              <PencilIcon aria-hidden="true" />
              Edit
            </DropdownMenuItem>
            <DropdownMenuItem onClick={onEditFeatures}>
              <SlidersHorizontalIcon aria-hidden="true" />
              Route features
            </DropdownMenuItem>
            <DropdownMenuItem onClick={handleCopyHostname}>
              <CopyIcon aria-hidden="true" />
              Copy hostname
            </DropdownMenuItem>
            {canDelete && (
              <>
                <DropdownMenuSeparator />
                <AlertDialogTrigger
                  render={<DropdownMenuItem variant="destructive" />}
                  nativeButton={false}
                >
                  <Trash2Icon aria-hidden="true" />
                  Delete
                </AlertDialogTrigger>
              </>
            )}
          </DropdownMenuContent>
        </DropdownMenu>
        <AlertDialogContent>
          <AlertDialogHeader>
            {/* The path belongs in the title. A hostname no longer identifies one
                domain: shop.com/ and shop.com/api are different rows serving
                different applications, and a confirmation that names only the host
                cannot tell the operator which of them is about to be deleted. */}
            <AlertDialogTitle>
              Delete {domain.hostname}
              {domain.path && domain.path !== "/" ? domain.path : ""}?
            </AlertDialogTitle>
            <AlertDialogDescription>
              This will delete the domain and all its route features.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                toast.promise(removeDomain.mutateAsync(domain.id), {
                  loading: "Deleting domain…",
                  success: "Domain deleted",
                  error: (err) => err.message,
                });
              }}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
