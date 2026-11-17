import { Fragment, type ReactNode, useMemo, useState } from "react";
import {
  type ColumnDef,
  type ColumnFiltersState,
  type OnChangeFn,
  type Row,
  type RowData,
  type SortingState,
  type Table as TableInstance,
  flexRender,
  getCoreRowModel,
  getExpandedRowModel,
  getFilteredRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  useReactTable,
} from "@tanstack/react-table";
import { ChevronDown, ChevronsUpDown, ChevronUp } from "lucide-react";

import { cn } from "@/lib/utils";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { DataTablePagination } from "@/components/ui/data-table/data-table-pagination";

// Per-column styling hooks, read while rendering header/body cells.
declare module "@tanstack/react-table" {
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  interface ColumnMeta<TData extends RowData, TValue> {
    className?: string;
    headerClassName?: string;
  }
}

export interface DetailPanelProps<TData> {
  row: Row<TData>;
  table: TableInstance<TData>;
}
export type DetailPanelFn<TData> = (
  props: DetailPanelProps<TData>,
) => ReactNode;

export interface CustomViewItemProps<TData> {
  row: Row<TData>;
  table: TableInstance<TData>;
}
export interface CustomViewProps<TData> {
  item: (props: CustomViewItemProps<TData>) => ReactNode;
  wrapperProps?: React.HTMLAttributes<HTMLDivElement>;
  loading?: ReactNode;
  empty?: ReactNode;
}

/**
 * Client mode paginates the full in-memory dataset; manual mode renders the
 * page the server already returned and drives Prev/Next via `onOffsetChange`.
 */
export type DataTablePaginationConfig =
  | { mode: "client"; pageSize?: number }
  | {
      mode: "manual";
      offset: number;
      pageSize: number;
      hasMore: boolean;
      onOffsetChange: (offset: number) => void;
    };

export interface DataTableProps<TData, TValue = unknown> {
  columns: ColumnDef<TData, TValue>[];
  data: TData[];
  isLoading?: boolean;
  loadingRowCount?: number;
  emptyMessage?: ReactNode;
  getRowId?: (row: TData, index: number) => string;
  onRowClick?: (row: TData) => void;
  rowClassName?: (row: TData) => string | undefined;
  /** Renders an expandable detail row; adds a leading expand toggle column. */
  renderDetailPanel?: DetailPanelFn<TData>;
  /**
   * Which rows can expand. Defaults to every row, which is what the existing callers rely
   * on; pass it when only some rows have a detail panel, so the others get no chevron
   * that opens onto nothing.
   */
  getRowCanExpand?: (row: Row<TData>) => boolean;
  /** Renders rows as cards/list (sharing sort/filter/pagination) instead of a table. */
  customView?: CustomViewProps<TData>;
  enableSorting?: boolean;
  /** Controlled sorting (e.g. driven by an external Select instead of headers). */
  sorting?: SortingState;
  onSortingChange?: OnChangeFn<SortingState>;
  /** Controlled client-side global filter (the page owns the search string). */
  globalFilter?: string;
  onGlobalFilterChange?: (value: string) => void;
  columnFilters?: ColumnFiltersState;
  onColumnFiltersChange?: OnChangeFn<ColumnFiltersState>;
  pagination?: DataTablePaginationConfig;
  className?: string;
}

export function DataTable<TData, TValue = unknown>({
  columns,
  data,
  isLoading,
  loadingRowCount = 5,
  emptyMessage = "No results.",
  getRowId,
  onRowClick,
  rowClassName,
  renderDetailPanel,
  getRowCanExpand = () => true,
  customView,
  enableSorting = false,
  sorting,
  onSortingChange,
  globalFilter,
  onGlobalFilterChange,
  columnFilters,
  onColumnFiltersChange,
  pagination,
  className,
}: DataTableProps<TData, TValue>) {
  const [internalSorting, setInternalSorting] = useState<SortingState>([]);
  const sortingState = sorting ?? internalSorting;
  const handleSortingChange = onSortingChange ?? setInternalSorting;

  // Prepend a chevron toggle column when rows can expand.
  const cols = useMemo<ColumnDef<TData, TValue>[]>(() => {
    if (!renderDetailPanel) return columns;
    const expander: ColumnDef<TData, TValue> = {
      id: "__expander",
      header: "",
      enableSorting: false,
      enableGlobalFilter: false,
      meta: { className: "w-9" },
      cell: ({ row }) =>
        !row.getCanExpand() ? null : (
          <button
            type="button"
            aria-label={row.getIsExpanded() ? "Collapse row" : "Expand row"}
            onClick={(e) => {
              e.stopPropagation();
              row.toggleExpanded();
            }}
            className="text-text-faint hover:text-foreground grid size-6 place-items-center rounded transition-colors"
          >
            <ChevronDown
              aria-hidden="true"
              className={cn(
                "size-4 transition-transform",
                row.getIsExpanded() && "rotate-180",
              )}
            />
          </button>
        ),
    };
    return [expander, ...columns];
  }, [columns, renderDetailPanel]);

  const filterControlled =
    globalFilter !== undefined || columnFilters !== undefined;

  // react-hooks/incompatible-library: TanStack Table's returned `table` object
  // carries methods the compiler can't prove are stable across renders, so it
  // skips memoizing this hook. That's a correct, permanent call on the
  // compiler's part, not a bug here — there's no alternative table API that
  // avoids it without dropping @tanstack/react-table.
  const table = useReactTable<TData>({
    data,
    columns: cols,
    getRowId,
    state: {
      sorting: sortingState,
      ...(globalFilter !== undefined ? { globalFilter } : {}),
      ...(columnFilters !== undefined ? { columnFilters } : {}),
    },
    onSortingChange: handleSortingChange,
    onColumnFiltersChange,
    onGlobalFilterChange: onGlobalFilterChange
      ? (updater) => {
          const next =
            typeof updater === "function"
              ? (updater as (old: string) => string)(globalFilter ?? "")
              : updater;
          onGlobalFilterChange(String(next ?? ""));
        }
      : undefined,
    enableSorting,
    globalFilterFn: "includesString",
    manualPagination: pagination?.mode === "manual",
    getCoreRowModel: getCoreRowModel(),
    ...(enableSorting ? { getSortedRowModel: getSortedRowModel() } : {}),
    ...(filterControlled ? { getFilteredRowModel: getFilteredRowModel() } : {}),
    ...(pagination?.mode === "client"
      ? { getPaginationRowModel: getPaginationRowModel() }
      : {}),
    ...(renderDetailPanel
      ? {
          getExpandedRowModel: getExpandedRowModel(),
          getRowCanExpand,
        }
      : {}),
    ...(pagination?.mode === "client" && pagination.pageSize
      ? { initialState: { pagination: { pageSize: pagination.pageSize } } }
      : {}),
  });

  const rows = table.getRowModel().rows;
  const colCount = cols.length;

  const footer =
    pagination && (rows.length > 0 || isLoading) ? (
      <DataTablePagination
        table={table}
        pagination={pagination}
        rowCount={rows.length}
      />
    ) : null;

  // ---- Card / list view -------------------------------------------------
  if (customView) {
    return (
      <div className={cn("space-y-3", className)}>
        {isLoading && rows.length === 0 ? (
          (customView.loading ?? <DefaultCardSkeleton />)
        ) : rows.length === 0 ? (
          (customView.empty ?? <EmptyState>{emptyMessage}</EmptyState>)
        ) : (
          <div {...customView.wrapperProps}>
            {rows.map((row) => (
              <Fragment key={row.id}>
                {customView.item({ row, table })}
              </Fragment>
            ))}
          </div>
        )}
        {footer}
      </div>
    );
  }

  // ---- Table view -------------------------------------------------------
  return (
    <div className={cn("space-y-3", className)}>
      <Table>
        <TableHeader>
          {table.getHeaderGroups().map((hg) => (
            <TableRow key={hg.id}>
              {hg.headers.map((header) => {
                const canSort = header.column.getCanSort();
                const sorted = header.column.getIsSorted();
                return (
                  <TableHead
                    key={header.id}
                    className={header.column.columnDef.meta?.headerClassName}
                  >
                    {header.isPlaceholder ? null : canSort ? (
                      <button
                        type="button"
                        onClick={header.column.getToggleSortingHandler()}
                        className="hover:text-foreground -mx-1 inline-flex items-center gap-1 rounded px-1"
                      >
                        {flexRender(
                          header.column.columnDef.header,
                          header.getContext(),
                        )}
                        {sorted === "asc" ? (
                          <ChevronUp aria-hidden="true" className="size-3.5" />
                        ) : sorted === "desc" ? (
                          <ChevronDown
                            aria-hidden="true"
                            className="size-3.5"
                          />
                        ) : (
                          <ChevronsUpDown
                            aria-hidden="true"
                            className="text-text-faint size-3.5"
                          />
                        )}
                      </button>
                    ) : (
                      flexRender(
                        header.column.columnDef.header,
                        header.getContext(),
                      )
                    )}
                  </TableHead>
                );
              })}
            </TableRow>
          ))}
        </TableHeader>
        <TableBody>
          {isLoading && rows.length === 0 ? (
            Array.from({ length: loadingRowCount }).map((_, i) => (
              <TableRow key={`skeleton-${i}`}>
                {cols.map((_c, ci) => (
                  <TableCell key={ci}>
                    <Skeleton className="h-4 w-full max-w-[12rem]" />
                  </TableCell>
                ))}
              </TableRow>
            ))
          ) : rows.length === 0 ? (
            <TableRow>
              <TableCell
                colSpan={colCount}
                className="text-muted-foreground h-24 text-center whitespace-normal"
              >
                {emptyMessage}
              </TableCell>
            </TableRow>
          ) : (
            rows.map((row) => (
              <Fragment key={row.id}>
                <TableRow
                  data-state={row.getIsExpanded() ? "expanded" : undefined}
                  onClick={
                    onRowClick ? () => onRowClick(row.original) : undefined
                  }
                  className={cn(
                    onRowClick && "cursor-pointer",
                    rowClassName?.(row.original),
                  )}
                >
                  {row.getVisibleCells().map((cell) => (
                    <TableCell
                      key={cell.id}
                      className={cell.column.columnDef.meta?.className}
                    >
                      {flexRender(
                        cell.column.columnDef.cell,
                        cell.getContext(),
                      )}
                    </TableCell>
                  ))}
                </TableRow>
                {row.getIsExpanded() && renderDetailPanel && (
                  <TableRow className="hover:bg-transparent">
                    <TableCell
                      colSpan={colCount}
                      className="bg-muted/30 whitespace-normal"
                    >
                      {renderDetailPanel({ row, table })}
                    </TableCell>
                  </TableRow>
                )}
              </Fragment>
            ))
          )}
        </TableBody>
      </Table>
      {footer}
    </div>
  );
}

function EmptyState({ children }: { children: ReactNode }) {
  return (
    <div className="text-muted-foreground rounded-lg border py-12 text-center text-sm">
      {children}
    </div>
  );
}

function DefaultCardSkeleton() {
  return (
    <div className="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
      {[1, 2, 3].map((i) => (
        <Skeleton key={i} className="h-28 w-full rounded-xl" />
      ))}
    </div>
  );
}
