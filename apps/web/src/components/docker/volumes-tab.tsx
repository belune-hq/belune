import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { Link } from "@tanstack/react-router";
import { DataTable, DataTableSearch } from "@/components/ui/data-table";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { useDockerVolumes } from "@/lib/hooks/use-docker";
import { formatDateTimeShort } from "@/lib/utils/format";
import type { DockerVolume } from "@/lib/types";
import { ManagedBadge } from "./shared";
import { sizeLabel } from "./utils";

function KindBadge({ kind }: { kind: DockerVolume["kind"] }) {
  if (kind === "data") {
    return (
      <Badge variant="light" className="font-normal">
        Data
      </Badge>
    );
  }
  if (kind === "cache") {
    return (
      <Badge variant="secondary" className="font-normal">
        Cache
      </Badge>
    );
  }
  return null;
}

export function DockerVolumesTab({ enabled }: { enabled: boolean }) {
  const { data, isPending } = useDockerVolumes(enabled);
  const [search, setSearch] = useState("");

  const columns = useMemo<ColumnDef<DockerVolume>[]>(
    () => [
      {
        accessorKey: "name",
        header: "Name",
        cell: ({ row }) => (
          <span className="font-mono text-xs break-all">
            {row.original.name}
          </span>
        ),
      },
      {
        accessorKey: "driver",
        header: "Driver",
        cell: ({ row }) => (
          <span className="text-sm">{row.original.driver || "—"}</span>
        ),
      },
      {
        accessorKey: "size",
        header: "Size",
        cell: ({ row }) => (
          <span className="tabular-nums">{sizeLabel(row.original.size)}</span>
        ),
      },
      {
        accessorKey: "ref_count",
        header: "In use",
        cell: ({ row }) => {
          const rc = row.original.ref_count;
          if (rc < 0) return <span className="text-text-faint">—</span>;
          return (
            <span className={rc === 0 ? "text-text-faint" : ""}>
              {rc === 0 ? "unused" : `${rc}`}
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
          if (row.original.adopted) {
            return (
              <div className="flex max-w-sm flex-col gap-1">
                <Badge variant="secondary">Adopted, no application</Badge>
                <p className="text-muted-foreground text-xs whitespace-normal">
                  Retained deliberately.{" "}
                  {row.original.missing ? (
                    "The volume is absent; this name remains protected if recreated."
                  ) : (
                    <>
                      To remove it, run{" "}
                      <code className="break-all">
                        docker volume rm {row.original.name}
                      </code>{" "}
                      on the host.
                    </>
                  )}
                </p>
              </div>
            );
          }
          return <ManagedBadge managed={managed} />;
        },
      },
      {
        id: "flags",
        header: "",
        cell: ({ row }) => (
          <div className="flex gap-1">
            <KindBadge kind={row.original.kind} />
            {row.original.adopted && <Badge variant="secondary">Adopted</Badge>}
            {row.original.missing && <Badge variant="outline">Missing</Badge>}
          </div>
        ),
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
          placeholder="Search by name or driver…"
          className="max-w-xs"
        />
        <DataTable
          columns={columns}
          data={data ?? []}
          isLoading={isPending}
          getRowId={(v) => v.name}
          enableSorting
          globalFilter={search}
          onGlobalFilterChange={setSearch}
          pagination={{ mode: "client", pageSize: 10 }}
          emptyMessage={
            search.trim()
              ? "No volumes match your search."
              : "No volumes on this host."
          }
        />
      </CardContent>
    </Card>
  );
}
