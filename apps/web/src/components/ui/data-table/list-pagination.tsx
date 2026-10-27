import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

/** The sizes offered, smallest first. The first one is also the default. */
export const LIST_PAGE_SIZES = [10, 25, 50, 100] as const;

interface ListPaginationProps {
  /** Zero-based. */
  page: number;
  pageSize: number;
  /** Rows available to page over right now — not the server's total. */
  total: number;
  onPageChange: (page: number) => void;
  onPageSizeChange: (size: number) => void;
  /**
   * More rows exist past `total` and can be fetched. Next stays enabled on the
   * last loaded page, and the count reads "of 50+" rather than claiming 50 is
   * all there is.
   */
  hasMore?: boolean;
  /** A fetch for those rows is in flight. */
  isLoadingMore?: boolean;
  /** What is being counted, plural: "clients", "detections". */
  noun: string;
}

/**
 * A pagination footer for a plain array, where DataTablePagination needs a
 * TanStack table instance. Same Previous / count / Next rhythm, plus the rows
 * per page control, which the lists that page in memory are the ones to need:
 * there is no server round trip to make a larger page expensive.
 */
export function ListPagination({
  page,
  pageSize,
  total,
  onPageChange,
  onPageSizeChange,
  hasMore = false,
  isLoadingMore = false,
  noun,
}: ListPaginationProps) {
  const pageCount = Math.max(1, Math.ceil(total / pageSize));
  // Nothing to page and nothing to resize. Checked against the smallest size
  // rather than the current one, or raising the size to 100 on a short list
  // would hide the control that put it there.
  if (total <= LIST_PAGE_SIZES[0] && !hasMore) return null;

  const start = total === 0 ? 0 : page * pageSize + 1;
  const end = Math.min(total, (page + 1) * pageSize);

  return (
    <div className="mt-3 flex flex-wrap items-center justify-between gap-3">
      <div className="flex items-center gap-2">
        <span className="text-muted-foreground text-sm">Rows per page</span>
        <Select
          value={String(pageSize)}
          onValueChange={(v) => onPageSizeChange(Number(v))}
        >
          <SelectTrigger aria-label="Rows per page" className="h-8 w-20">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {LIST_PAGE_SIZES.map((n) => (
              <SelectItem key={n} value={String(n)}>
                {n}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <div className="flex items-center gap-3">
        <span className="text-muted-foreground text-sm tabular-nums">
          {start}–{end} of {total}
          {hasMore && "+"} {noun}
        </span>
        <Button
          variant="outline"
          size="sm"
          disabled={page === 0}
          onClick={() => onPageChange(page - 1)}
        >
          Previous
        </Button>
        <Button
          variant="outline"
          size="sm"
          disabled={(page >= pageCount - 1 && !hasMore) || isLoadingMore}
          onClick={() => onPageChange(page + 1)}
        >
          {isLoadingMore ? "Loading…" : "Next"}
        </Button>
      </div>
    </div>
  );
}
