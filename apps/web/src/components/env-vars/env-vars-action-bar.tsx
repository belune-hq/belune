import {
  ArrowDownAZIcon,
  ChevronsDownUp,
  ChevronsUpDown,
  ClockIcon,
  HistoryIcon,
  Plus,
  Save,
  Search,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Tooltip,
  TooltipContent,
  TooltipPositioner,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupInput,
} from "../ui/input-group";
import type { EnvVarSortKey } from "./env-vars-sort";

// Rendered above the list (with search + collapse-all + sort) and repeated
// below it (Add + Save only) so long lists can be acted on without scrolling
// back up.
export function EnvVarsActionBar({
  variant,
  search,
  onSearchChange,
  allCollapsed,
  onToggleCollapseAll,
  sortKey,
  onSortKeyChange,
  onAdd,
  onSave,
  saving,
}: {
  variant: "top" | "bottom";
  search?: string;
  onSearchChange?: (v: string) => void;
  allCollapsed?: boolean;
  onToggleCollapseAll?: () => void;
  sortKey?: EnvVarSortKey;
  onSortKeyChange?: (v: EnvVarSortKey) => void;
  onAdd: () => void;
  onSave: () => void;
  saving: boolean;
}) {
  return (
    <div className="flex justify-between">
      {variant === "top" && (
        <div className="flex items-center gap-2">
          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  size="icon-sm"
                  variant="outline"
                  aria-label={allCollapsed ? "Expand all" : "Collapse all"}
                  onClick={onToggleCollapseAll}
                  className="gap-1.5"
                />
              }
            >
              {allCollapsed ? (
                <ChevronsUpDown className="size-3.5" />
              ) : (
                <ChevronsDownUp className="size-3.5" />
              )}
            </TooltipTrigger>
            <TooltipPositioner>
              <TooltipContent>
                {allCollapsed ? "Expand all" : "Collapse all"}
              </TooltipContent>
            </TooltipPositioner>
          </Tooltip>
          <InputGroup className="w-60">
            <InputGroupInput
              placeholder="Search..."
              value={search}
              onChange={(e) => onSearchChange?.(e.target.value)}
            />
            <InputGroupAddon>
              <Search />
            </InputGroupAddon>
          </InputGroup>
          {sortKey && (
            <Select
              value={sortKey}
              onValueChange={(v) => onSortKeyChange?.(v as EnvVarSortKey)}
            >
              <SelectTrigger
                className="w-40 capitalize"
                aria-label="Sort variables"
              >
                <span className="text-text-faint mr-1">Sort:</span>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem
                  value="name"
                  icon={<ArrowDownAZIcon />}
                  className="capitalize"
                >
                  Name
                </SelectItem>
                <SelectItem
                  value="created"
                  icon={<ClockIcon />}
                  className="capitalize"
                >
                  Created
                </SelectItem>
                <SelectItem
                  value="updated"
                  icon={<HistoryIcon />}
                  className="capitalize"
                >
                  Updated
                </SelectItem>
              </SelectContent>
            </Select>
          )}
        </div>
      )}
      {variant === "bottom" && <div />}

      <div className="flex items-center gap-2">
        <Button size="sm" variant="outline" onClick={onAdd} className="gap-1.5">
          <Plus className="size-3.5" />
          Add
        </Button>
        <Button size="sm" onClick={onSave} disabled={saving}>
          <Save className="size-3.5" />
          {saving ? "Saving..." : "Save"}
        </Button>
      </div>
    </div>
  );
}
