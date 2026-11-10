import type { ScheduledTaskRun } from "@/lib/types";
import { describeRunSite } from "./scheduled-task-status";

export function RunSite({
  run,
}: {
  run: Pick<ScheduledTaskRun, "status" | "executed_in">;
}) {
  const site = describeRunSite(run);
  if (!site) return null;
  return (
    <p className="text-text-faint text-xs">
      {run.executed_in ? "Ran in " : ""}
      <span className={run.executed_in ? "text-foreground font-mono" : ""}>
        {site.label}
      </span>
      {site.detail && <span className="font-mono"> · {site.detail}</span>}
    </p>
  );
}
