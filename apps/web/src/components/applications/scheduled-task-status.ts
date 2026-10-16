import type { ScheduledTask, ScheduledTaskRun } from "@/lib/types";

// Tone and label for a scheduled-task run. Every status is mapped explicitly:
// the backup sheets' statusTone() ends in a `default:` that returns the
// in-progress tone, which would paint 'skipped' and 'timed_out' as running.
export function runTone(status: ScheduledTaskRun["status"]): string {
  switch (status) {
    case "succeeded":
      return "text-status-ready";
    case "failed":
    case "timed_out":
      return "text-status-error";
    case "running":
      return "text-status-building";
    // A skip is normal operation; colouring it as failure trains people to
    // ignore real failures.
    case "skipped":
      return "text-text-faint";
  }
}

export function runLabel(
  run: Pick<ScheduledTaskRun, "status" | "exit_code">,
  task: Pick<ScheduledTask, "timeout_seconds">,
): string {
  switch (run.status) {
    case "timed_out":
      return `Timed out (${task.timeout_seconds}s)`;
    case "failed":
      return run.exit_code != null
        ? `Failed (exit ${run.exit_code})`
        : "Failed";
    case "succeeded":
      return "Succeeded";
    case "running":
      return "Running";
    case "skipped":
      return "Skipped";
  }
}
