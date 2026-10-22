import * as React from "react";
import { type DateRange } from "react-day-picker";
import { format } from "date-fns";
import { CalendarIcon, XIcon } from "lucide-react";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Calendar } from "@/components/ui/calendar";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";

interface DateRangePickerProps {
  value?: { from?: string; to?: string };
  onChange: (range: { from?: string; to?: string }) => void;
  placeholder?: string;
  className?: string;
}

function DateRangePicker({
  value,
  onChange,
  placeholder = "Pick a date range",
  className,
}: DateRangePickerProps) {
  const [open, setOpen] = React.useState(false);
  const [firstDate, setFirstDate] = React.useState<Date | undefined>(undefined);

  const committed: DateRange = {
    from: value?.from ? new Date(value.from) : undefined,
    to: value?.to ? new Date(value.to) : undefined,
  };

  const hasValue = !!(committed.from && committed.to);

  const calendarSelected: DateRange = firstDate
    ? { from: firstDate, to: undefined }
    : committed;

  function handleSelect(_range: DateRange | undefined, selectedDay: Date) {
    if (!firstDate) {
      setFirstDate(selectedDay);
    } else {
      const [start, end] =
        firstDate <= selectedDay
          ? [firstDate, selectedDay]
          : [selectedDay, firstDate];

      // Set end to 23:59:59.999 so the full day is included in the filter.
      const endOfDay = new Date(end);
      endOfDay.setHours(23, 59, 59, 999);

      onChange({ from: start.toISOString(), to: endOfDay.toISOString() });
      setFirstDate(undefined);
      setOpen(false);
    }
  }

  function handleOpenChange(next: boolean) {
    if (!next) setFirstDate(undefined);
    setOpen(next);
  }

  function handleClear() {
    onChange({});
    setFirstDate(undefined);
  }

  return (
    <div className={cn("relative flex items-center", className)}>
      <Popover open={open} onOpenChange={handleOpenChange}>
        <PopoverTrigger
          render={
            <Button
              variant="outline"
              className={cn(
                "h-9 w-full justify-start gap-2 text-left font-normal",
                hasValue ? "pr-8" : "",
                !hasValue && "text-muted-foreground",
              )}
            />
          }
        >
          <CalendarIcon className="size-4 shrink-0" />
          <span className="flex-1 truncate text-sm">
            {committed.from && committed.to ? (
              <>
                {format(committed.from, "MMM d, yyyy")} –{" "}
                {format(committed.to, "MMM d, yyyy")}
              </>
            ) : (
              placeholder
            )}
          </span>
        </PopoverTrigger>
        <PopoverContent className="w-auto p-0" align="start">
          <Calendar
            key={String(open)}
            mode="range"
            selected={calendarSelected}
            onSelect={handleSelect}
            numberOfMonths={2}
            defaultMonth={committed.from ?? new Date()}
            disabled={{ after: new Date() }}
          />
          {firstDate && (
            <p className="text-muted-foreground pb-3 text-center text-xs">
              Now pick an end date
            </p>
          )}
        </PopoverContent>
      </Popover>

      {/* Clear button sits outside PopoverTrigger to avoid toggling the popover */}
      {hasValue && (
        <button
          onClick={handleClear}
          className="text-muted-foreground hover:text-foreground absolute right-2.5 flex items-center justify-center"
          aria-label="Clear date range"
        >
          <XIcon className="size-3.5" />
        </button>
      )}
    </div>
  );
}

export { DateRangePicker };
