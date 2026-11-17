import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import type { ScheduledTask, ScheduledTaskRun } from "@/lib/types";

const tasks: { current: ScheduledTask[] } = { current: [] };

vi.mock("@/lib/hooks/use-scheduled-tasks", () => {
  const mutation = () => ({ mutateAsync: vi.fn(), isPending: false });
  return {
    useScheduledTasks: () => ({ data: tasks.current, isLoading: false }),
    useUpdateScheduledTask: mutation,
    useDeleteScheduledTask: mutation,
    useRunScheduledTask: mutation,
    useCreateScheduledTask: mutation,
    useScheduledTaskRuns: () => ({ data: [], isLoading: false }),
  };
});

import { ScheduledTasksSection } from "./scheduled-tasks-section";

const run = (over: Partial<ScheduledTaskRun>): ScheduledTaskRun => ({
  id: "r1",
  trigger: "schedule",
  status: "succeeded",
  started_at: "2026-11-16T03:00:00Z",
  finished_at: "2026-11-16T03:00:01Z",
  executed_in: {
    container_name: "app-web-2-5726bda2",
    process_type: "web",
    idx: 2,
  },
  ...over,
});

const task = (latest?: ScheduledTaskRun): ScheduledTask => ({
  id: "t1",
  application_id: "a1",
  name: "nightly",
  command: "echo hi",
  cron: "0 3 * * *",
  timezone: "UTC",
  enabled: true,
  timeout_seconds: 30,
  keep_runs: 5,
  latest_run: latest,
  created_at: "2026-11-01T00:00:00Z",
  updated_at: "2026-11-01T00:00:00Z",
});

function renderRow(latest?: ScheduledTaskRun) {
  tasks.current = [task(latest)];
  return render(<ScheduledTasksSection projectId="p1" applicationId="a1" />);
}

describe("a scheduled task row", () => {
  it("says which container the last run executed in", () => {
    renderRow(run({}));
    expect(screen.getByText("web-2")).toBeTruthy();
    expect(screen.getByText(/app-web-2-5726bda2/)).toBeTruthy();
  });

  it("says nothing about a container for a skipped run: it reached none", () => {
    renderRow(
      run({
        status: "skipped",
        skip_reason: "no running container",
        executed_in: null,
      }),
    );
    expect(screen.getByText(/last skipped: no running container/)).toBeTruthy();
    expect(screen.queryByText(/Container not recorded/)).toBeNull();
    expect(screen.queryByText(/^Ran in/)).toBeNull();
  });

  it("does not invent a site for a task that has never run", () => {
    renderRow(undefined);
    expect(screen.queryByText(/Container not recorded/)).toBeNull();
    expect(screen.queryByText(/Ran in/)).toBeNull();
  });
});
