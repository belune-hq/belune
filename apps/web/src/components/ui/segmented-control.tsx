"use client";

import { ToggleGroup as ToggleGroupPrimitive } from "@base-ui/react/toggle-group";
import { Toggle as TogglePrimitive } from "@base-ui/react/toggle";

import { cn } from "@/lib/utils";

/**
 * Segmented control: a single-select row of pills on an inset track, where the
 * active option reads as a raised card. Built on Base UI's ToggleGroup/Toggle
 * so it gets single-select semantics and roving keyboard focus for free.
 *
 * Unlike the array-based ToggleGroup, this exposes a string value/onValueChange
 * and never lets the user deselect (there is always one active segment).
 *
 * - `size`: "default" (card-sized, e.g. settings) or "sm" (compact toolbars).
 * - `fullWidth`: stretch to the container with equal-width segments.
 *
 * Each segment's `value` must be a non-empty string — Base UI's Toggle treats
 * an empty value as unset, so it would never highlight. For an "All" segment,
 * use a token like "all" and map it at the call site.
 */
function SegmentedControl({
  value,
  onValueChange,
  size = "default",
  fullWidth = false,
  className,
  ...props
}: Omit<
  ToggleGroupPrimitive.Props,
  "value" | "defaultValue" | "onValueChange"
> & {
  value: string;
  onValueChange: (value: string) => void;
  size?: "sm" | "default";
  fullWidth?: boolean;
}) {
  return (
    <ToggleGroupPrimitive
      data-slot="segmented-control"
      data-size={size}
      data-fullwidth={fullWidth ? "true" : undefined}
      value={[value]}
      onValueChange={(next) => {
        const v = next[0];
        // A click on the active pill yields an empty array (deselect); ignore
        // it so a segmented control always keeps exactly one selection. An
        // empty-string value is a legitimate option (e.g. an "All" segment).
        if (v != null) onValueChange(String(v));
      }}
      className={cn(
        "group/segmented bg-elev flex w-fit items-center gap-1 rounded-lg p-1 data-[fullwidth=true]:w-full data-[size=sm]:p-0.5",
        className,
      )}
      {...props}
    />
  );
}

function SegmentedControlItem({ className, ...props }: TogglePrimitive.Props) {
  return (
    <TogglePrimitive
      data-slot="segmented-control-item"
      className={cn(
        "text-muted-foreground hover:text-foreground focus-visible:ring-ring/50 aria-pressed:bg-card aria-pressed:text-foreground aria-pressed:ring-border-strong flex items-center justify-center gap-2 rounded-md text-sm font-medium whitespace-nowrap transition-colors outline-none group-data-[fullwidth=true]/segmented:flex-1 group-data-[size=default]/segmented:px-3 group-data-[size=default]/segmented:py-2 group-data-[size=sm]/segmented:px-2.5 group-data-[size=sm]/segmented:py-1 focus-visible:ring-2 disabled:pointer-events-none disabled:opacity-50 aria-pressed:shadow-sm aria-pressed:ring-1 [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
        className,
      )}
      {...props}
    />
  );
}

export { SegmentedControl, SegmentedControlItem };
