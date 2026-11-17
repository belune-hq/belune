import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { Link } from "@tanstack/react-router";
import { DataTable, DataTableSearch } from "@/components/ui/data-table";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { useDockerContainers } from "@/lib/hooks/use-docker";
import { formatDateTimeShort } from "@/lib/utils/format";
import type { DockerContainer } from "@/lib/types";
import { ManagedBadge } from "./shared";
import { shortId } from "./utils";

/** Docker container states → badge tone. Running is emphasised; the rest muted. */
function stateVariant(status: string): "default" | "secondary" | "destructive" {
  const s = status.toLowerCase();
  if (s === "running") return "default";
  if (s === "dead" || s === "exited") return "destructive";
  return "secondary";
}

export function DockerContainersTab({
  enabled,
  initialSearch = "",
}: {
  enabled: boolean;
  initialSearch?: string;
}) {
  const { data, isPending } = useDockerContainers(enabled);
  const [search, setSearch] = useState(initialSearch);

  const columns = useMemo<ColumnDef<DockerContainer>[]>(
    () => [
      {
        accessorKey: "name",
        header: "Name",
        cell: ({ row }) => (
          <div className="flex flex-col">
            <span className="font-medium">{row.original.name || "—"}</span>
            <span className="text-text-faint font-mono text-xs">
              {shortId(row.original.id)}
            </span>
          </div>
        ),
      },
      {
        accessorKey: "image",
        header: "Image",
        cell: ({ row }) => (
          <span className="font-mono text-xs break-all">
            {row.original.image}
          </span>
        ),
      },
      {
        accessorKey: "status",
        header: "State",
        cell: ({ row }) => (
          <Badge
            variant={stateVariant(row.original.status)}
            className="capitalize"
          >
            {row.original.status || "unknown"}
          </Badge>
        ),
      },
      {
        id: "ports",
        header: "Ports",
        cell: ({ row }) => {
          const ports = Object.entries(row.original.ports ?? {});
          if (ports.length === 0)
            return <span className="text-text-faint">—</span>;
          return (
            <span className="font-mono text-xs">
              {ports.map(([host, ctr]) => `${host}→${ctr}`).join(", ")}
            </span>
          );
        },
      },
      {
        id: "owner",
        header: "Owner",
        cell: ({ row }) => {
          const { owner, managed } = row.original;
          if (owner?.type === "application") {
            return (
              <Link
                to="/projects/$projectId/applications/$applicationId"
                params={{
                  projectId: owner.project_id,
                  applicationId: owner.id,
                }}
                className="text-brand-text text-sm hover:underline"
              >
                {owner.name}
              </Link>
            );
          }
          if (owner?.type === "database") {
            return (
              <Link
                to="/projects/$projectId"
                params={{ projectId: owner.project_id }}
                className="text-brand-text text-sm hover:underline"
              >
                {owner.name}
              </Link>
            );
          }
          return <ManagedBadge managed={managed} />;
        },
      },
      {
        accessorKey: "created_at",
        header: "Created",
        cell: ({ row }) =>
          row.original.created_at ? (
            <span className="text-muted-foreground text-sm">
              {formatDateTimeShort(row.original.created_at)}
            </span>
          ) : (
            <span className="text-text-faint">—</span>
          ),
      },
    ],
    [],
  );

  return (
    <Card>
      <CardContent className="space-y-3">
        <DataTableSearch
          value={search}
          onChange={setSearch}
          placeholder="Search by name, image, or state…"
          className="max-w-xs"
        />
        <DataTable
          columns={columns}
          data={data ?? []}
          isLoading={isPending}
          getRowId={(c) => c.id}
          enableSorting
          globalFilter={search}
          onGlobalFilterChange={setSearch}
          pagination={{ mode: "client", pageSize: 10 }}
          emptyMessage={
            search.trim()
              ? "No containers match your search."
              : "No containers on this host."
          }
        />
      </CardContent>
    </Card>
  );
}
