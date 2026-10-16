import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";

const noop = { mutateAsync: vi.fn(), isPending: false };
vi.mock("@/lib/hooks/use-scheduled-tasks", () => ({
  useCreateScheduledTask: () => noop,
  useUpdateScheduledTask: () => noop,
}));
vi.mock("@/lib/hooks/use-database-backup-configs", () => ({
  useCreateBackupConfig: () => noop,
  useUpdateBackupConfig: () => noop,
}));
vi.mock("@/lib/hooks/use-volume-backups", () => ({
  useCreateVolumeBackupConfig: () => noop,
  useUpdateVolumeBackupConfig: () => noop,
}));
vi.mock("@/lib/hooks/use-volumes", () => ({
  useVolumes: () => ({ data: [] }),
}));
vi.mock("@/lib/hooks/use-backup-destinations", () => ({
  useBackupDestinations: () => ({ data: [] }),
}));
vi.mock("sonner", () => ({ toast: { promise: vi.fn(), error: vi.fn() } }));

import { ScheduledTaskForm } from "./scheduled-task-form";
import { BackupConfigFormDialog } from "@/components/databases/backup-config-form-dialog";
import { VolumeBackupConfigForm } from "./volume-backup-config-form";

// Rendered text and the invalid flag only; whether a person can SEE the message
// is the browser drill's job (jsdom has no layout engine).
const forms: {
  name: string;
  render: () => void;
  input: () => HTMLElement;
  // The wrapper that carries data-invalid for the field.
  field: (el: HTMLElement) => HTMLElement | null;
}[] = [
  {
    name: "task form",
    render: () =>
      render(
        <ScheduledTaskForm
          projectId="p"
          applicationId="a"
          task={null}
          open
          onOpenChange={() => {}}
        />,
      ),
    input: () => screen.getByLabelText("Schedule"),
    field: (el) => el.closest("[data-invalid]"),
  },
  {
    name: "database backup form",
    render: () =>
      render(
        <BackupConfigFormDialog
          projectId="p"
          databaseId="d"
          open
          onOpenChange={() => {}}
        />,
      ),
    input: () => screen.getByPlaceholderText(/Custom cron/),
    field: (el) => el.closest("[data-invalid]"),
  },
  {
    name: "volume backup form",
    render: () =>
      render(
        <VolumeBackupConfigForm
          projectId="p"
          applicationId="a"
          open
          onOpenChange={() => {}}
        />,
      ),
    input: () => screen.getByLabelText("Cron expression"),
    field: (el) => el.closest("[data-invalid]"),
  },
];

describe.each(forms)("$name schedule field", (f) => {
  it("flags an out-of-range expression once the field has been left", () => {
    f.render();
    const input = f.input();
    fireEvent.change(input, { target: { value: "0 99 * * *" } });
    fireEvent.blur(input);
    expect(screen.getByText("Not a valid cron expression")).toBeTruthy();
    expect(f.field(input)?.getAttribute("data-invalid")).toBe("true");
  });

  it("does not flash an error while a valid expression is still being typed", () => {
    f.render();
    const input = f.input();
    for (const partial of ["0", "0 3", "0 3 *", "0 3 * *", "0 3 * * *"]) {
      fireEvent.change(input, { target: { value: partial } });
      expect(screen.queryByText("Not a valid cron expression")).toBeNull();
      expect(screen.queryByText("Use a 5-field cron expression")).toBeNull();
      expect(f.field(input)?.getAttribute("data-invalid")).not.toBe("true");
    }
  });
});

describe("task schedule field", () => {
  it("rejects a sub-minute @every once visited, like the server does", () => {
    forms[0].render();
    const input = forms[0].input();
    fireEvent.change(input, { target: { value: "@every 30s" } });
    fireEvent.blur(input);
    expect(
      screen.getByText("Schedules must be at least one minute apart"),
    ).toBeTruthy();
  });
});
