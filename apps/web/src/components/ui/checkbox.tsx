import { Checkbox as CheckboxPrimitive } from "@base-ui/react/checkbox";
import { CheckIcon, MinusIcon } from "lucide-react";

import { cn } from "@/lib/utils";

/**
 * Form checkbox — the counterpart to <Switch> in the toggle convention
 * documented on that component:
 *   - <Switch>   — flipping it acts now; there is no submit button.
 *   - <Checkbox> — a form field, persisted only when the form is submitted.
 *
 * This exists so that distinction costs nothing visually. Form fields were
 * previously raw <input type="checkbox">, which renders as the unstyled browser
 * control and looked out of place next to the rest of the UI — the reason to
 * reach for a Switch was appearance, not meaning, and reaching for one in a form
 * tells the user their change is already in effect when it is not.
 */
function Checkbox({ className, ...props }: CheckboxPrimitive.Root.Props) {
  return (
    <CheckboxPrimitive.Root
      data-slot="checkbox"
      className={cn(
        // Explicit radius rather than rounded-sm: the shared scale is tuned for
        // cards and inputs (--radius-sm resolves to 6px), which on a 16px box is
        // 37% of its width and renders as a circle — indistinguishable from a
        // radio button. 4px keeps it legibly square at this size.
        "peer border-border-strong flex size-4 shrink-0 cursor-pointer items-center justify-center rounded-[4px] border shadow-xs transition-colors outline-none",
        "focus-visible:border-ring focus-visible:ring-ring/50 focus-visible:ring-[3px]",
        "data-[checked]:bg-primary data-[checked]:border-primary data-[checked]:text-primary-foreground",
        "data-[indeterminate]:bg-primary data-[indeterminate]:border-primary data-[indeterminate]:text-primary-foreground",
        "disabled:cursor-not-allowed disabled:opacity-50",
        "aria-invalid:border-destructive aria-invalid:ring-destructive/20",
        className,
      )}
      {...props}
    >
      <CheckboxPrimitive.Indicator
        data-slot="checkbox-indicator"
        className="flex items-center justify-center text-current"
        // Rendered for the indeterminate state too, which has no check mark.
        render={(indicatorProps, state) => (
          <span {...indicatorProps}>
            {state.indeterminate ? (
              <MinusIcon className="size-3.5" strokeWidth={3} />
            ) : (
              <CheckIcon className="size-3.5" strokeWidth={3} />
            )}
          </span>
        )}
      />
    </CheckboxPrimitive.Root>
  );
}

export { Checkbox };
