import { describeCron } from "@/lib/utils/schedule";
import {
  Tooltip,
  TooltipContent,
  TooltipPositioner,
  TooltipTrigger,
} from "@/components/ui/tooltip";

// "[schedule] ([timezone])" with the raw expression in a tooltip. The trigger is
// focusable so the expression is reachable by keyboard. Touch is the weak spot,
// which is acceptable only because every edit form shows the raw expression.
export function ScheduleText({
  cron,
  timezone,
}: {
  cron: string;
  timezone: string;
}) {
  return (
    <Tooltip>
      <TooltipTrigger render={<span tabIndex={0} className="cursor-help" />}>
        {describeCron(cron)} ({timezone})
      </TooltipTrigger>
      <TooltipPositioner>
        <TooltipContent>
          <code className="font-mono">{cron}</code>
        </TooltipContent>
      </TooltipPositioner>
    </Tooltip>
  );
}
