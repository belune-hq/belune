import * as React from "react";
import { Select as SelectPrimitive } from "@base-ui/react/select";
import { ChevronDownIcon, CheckIcon } from "lucide-react";
import { cn } from "@/lib/utils";

type SelectItemData = {
  label: React.ReactNode;
  icon?: React.ReactNode;
};

// Maps each item's value -> { label, icon } so the trigger (<SelectValue>) can
// render both the label and its leading icon for the selected option. Base UI's
// own `items` prop only carries labels, so we thread the icons through context.
const SelectItemsContext = React.createContext<Record<string, SelectItemData>>(
  {},
);

// Base UI's <Select.Value> resolves its display label from the root's `items`
// map (value -> label); without it, it falls back to rendering the raw value.
// Walk the SelectItem children to build that map so every call site shows the
// label — matching the Radix/shadcn behaviour — without passing `items` by hand.
// We also capture each item's `icon` so the selected value can show it too.
function collectSelectItems(
  children: React.ReactNode,
  acc: Record<string, SelectItemData>,
) {
  React.Children.forEach(children, (child) => {
    if (!React.isValidElement(child)) return;
    if (child.type === SelectItem) {
      const {
        value,
        children: label,
        icon,
      } = child.props as {
        value?: unknown;
        children?: React.ReactNode;
        icon?: React.ReactNode;
      };
      if (value != null) acc[String(value)] = { label, icon };
      return;
    }
    const nested = (child.props as { children?: React.ReactNode })?.children;
    if (nested != null) collectSelectItems(nested, acc);
  });
  return acc;
}

function Select<Value = string, Multiple extends boolean | undefined = false>({
  items,
  children,
  ...props
}: SelectPrimitive.Root.Props<Value, Multiple>) {
  const data = React.useMemo(
    () => collectSelectItems(children, {}),
    [children],
  );
  // Respect an explicit `items` prop; otherwise derive the label map Base UI
  // needs from the collected children.
  const derivedItems = React.useMemo(() => {
    if (items != null) return items;
    const labels: Record<string, React.ReactNode> = {};
    for (const [value, { label }] of Object.entries(data)) {
      labels[value] = label;
    }
    return Object.keys(labels).length > 0 ? labels : undefined;
  }, [items, data]);

  return (
    <SelectItemsContext.Provider value={data}>
      <SelectPrimitive.Root data-slot="select" items={derivedItems} {...props}>
        {children}
      </SelectPrimitive.Root>
    </SelectItemsContext.Provider>
  );
}

function SelectTrigger({
  className,
  children,
  ...props
}: SelectPrimitive.Trigger.Props) {
  return (
    <SelectPrimitive.Trigger
      data-slot="select-trigger"
      className={cn(
        "border-input bg-background focus:border-ring focus:ring-ring/50 data-[popup-open]:border-ring data-[popup-open]:ring-ring/50 aria-invalid:border-destructive aria-invalid:ring-destructive/20 dark:aria-invalid:border-destructive/50 dark:aria-invalid:ring-destructive/40 flex h-9 w-full items-center justify-between gap-2 rounded-md border px-3 py-2 text-sm shadow-xs transition-[color,box-shadow] outline-none focus:ring-[3px] disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:ring-3 data-[popup-open]:ring-[3px]",
        className,
      )}
      {...props}
    >
      {children}
      <SelectPrimitive.Icon>
        <ChevronDownIcon className="size-4 opacity-50 transition-transform duration-200 data-[popup-open]:rotate-180" />
      </SelectPrimitive.Icon>
    </SelectPrimitive.Trigger>
  );
}

function SelectValue({
  className,
  children,
  placeholder,
  ...props
}: SelectPrimitive.Value.Props) {
  const data = React.useContext(SelectItemsContext);
  return (
    <SelectPrimitive.Value
      data-slot="select-value"
      className={cn(
        "flex min-w-0 items-center gap-2 [&>svg]:size-4 [&>svg]:shrink-0",
        className,
      )}
      placeholder={placeholder}
      {...props}
    >
      {/* Honour an explicit child; otherwise render the selected item's icon
          (from context) alongside its label. An empty-string value can be a
          real option (e.g. "All statuses"), so resolve against the item map
          first and only fall back to the placeholder when nothing matches. */}
      {children != null
        ? children
        : (value: unknown) => {
            if (Array.isArray(value)) return placeholder ?? null;
            const entry = data[value == null ? "" : String(value)];
            if (entry) {
              return (
                <>
                  {entry.icon}
                  <span className="truncate">{entry.label}</span>
                </>
              );
            }
            return value == null || value === ""
              ? (placeholder ?? null)
              : String(value);
          }}
    </SelectPrimitive.Value>
  );
}

function SelectContent({
  className,
  children,
  align = "start",
  sideOffset = 4,
  ...props
}: SelectPrimitive.Popup.Props &
  Pick<SelectPrimitive.Positioner.Props, "align" | "sideOffset">) {
  return (
    <SelectPrimitive.Portal>
      <SelectPrimitive.Positioner
        className="isolate z-50 outline-none"
        align={align}
        sideOffset={sideOffset}
        // Anchor the popup below the trigger like a standard popover. Base UI's
        // default macOS-style mode (overlay the selected item on the trigger)
        // relies on scroll math that misfires after a dialog re-renders,
        // placing the popup far off-screen (e.g. the Quotas target select after
        // switching scope). Standard anchoring is predictable and web-native.
        alignItemWithTrigger={false}
      >
        <SelectPrimitive.Popup
          data-slot="select-content"
          className={cn(
            "bg-popover text-popover-foreground ring-foreground/10 data-open:animate-in data-open:fade-in-0 data-open:zoom-in-95 data-closed:animate-out data-closed:fade-out-0 data-closed:zoom-out-95 z-50 max-h-[min(var(--available-height,320px),320px)] w-fit max-w-[24rem] min-w-(--anchor-width) origin-(--transform-origin) overflow-x-hidden overflow-y-auto rounded-lg p-1 shadow-md ring-1 duration-100 outline-none data-closed:overflow-hidden",
            className,
          )}
          {...props}
        >
          <SelectPrimitive.List>{children}</SelectPrimitive.List>
        </SelectPrimitive.Popup>
      </SelectPrimitive.Positioner>
    </SelectPrimitive.Portal>
  );
}

function SelectItem({
  className,
  children,
  icon,
  ...props
}: SelectPrimitive.Item.Props & { icon?: React.ReactNode }) {
  return (
    <SelectPrimitive.Item
      data-slot="select-item"
      className={cn(
        "focus:bg-foreground/10 focus:text-accent-foreground relative flex cursor-default items-center gap-2 rounded-md py-1.5 pr-8 pl-2 text-sm outline-none select-none data-disabled:pointer-events-none data-disabled:opacity-50 [&_svg]:size-4 [&_svg]:shrink-0",
        className,
      )}
      {...props}
    >
      <span className="pointer-events-none absolute right-2 flex items-center justify-center">
        <SelectPrimitive.ItemIndicator>
          <CheckIcon className="size-4" />
        </SelectPrimitive.ItemIndicator>
      </span>
      {/* Leading icon sits outside ItemText so it lays out inline with the
          label (and stays out of the trigger's selected-value text). */}
      {icon}
      <SelectPrimitive.ItemText>{children}</SelectPrimitive.ItemText>
    </SelectPrimitive.Item>
  );
}

function SelectSeparator({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="select-separator"
      className={cn("bg-border -mx-1 my-1 h-px", className)}
      {...props}
    />
  );
}

function SelectGroup({ ...props }: SelectPrimitive.Group.Props) {
  return <SelectPrimitive.Group data-slot="select-group" {...props} />;
}

function SelectGroupLabel({
  className,
  ...props
}: SelectPrimitive.GroupLabel.Props) {
  return (
    <SelectPrimitive.GroupLabel
      data-slot="select-group-label"
      className={cn(
        "text-muted-foreground px-2 py-1 text-xs font-medium",
        className,
      )}
      {...props}
    />
  );
}

export {
  Select,
  SelectTrigger,
  SelectValue,
  SelectContent,
  SelectItem,
  SelectSeparator,
  SelectGroup,
  SelectGroupLabel,
};
