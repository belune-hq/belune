import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";

import { RunSite } from "./scheduled-run-site";
import { describeRunSite } from "./scheduled-task-status";

const site = {
  container_name: "app-web-2-5726bda2",
  process_type: "web",
  idx: 2,
};

describe("RunSite", () => {
  it("renders the recorded container: stable label, then the name it had", () => {
    render(<RunSite run={{ status: "succeeded", executed_in: site }} />);
    expect(screen.getByText("web-2")).toBeTruthy();
    expect(screen.getByText(/app-web-2-5726bda2/)).toBeTruthy();
  });

  it("says 'not recorded' for a finished run without one, not an empty cell", () => {
    render(<RunSite run={{ status: "failed", executed_in: null }} />);
    expect(screen.getByText("Container not recorded")).toBeTruthy();
  });

  it("does not claim a run in flight is unrecorded", () => {
    render(<RunSite run={{ status: "running", executed_in: null }} />);
    expect(screen.getByText("Finding container…")).toBeTruthy();
    expect(screen.queryByText("Container not recorded")).toBeNull();
  });

  it("says nothing for a skipped run: it executed nowhere", () => {
    const { container } = render(
      <RunSite run={{ status: "skipped", executed_in: null }} />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("falls back to the container name when the process type was not recorded", () => {
    expect(
      describeRunSite({
        status: "succeeded",
        executed_in: { ...site, process_type: "" },
      }),
    ).toEqual({ label: "app-web-2-5726bda2" });
  });
});
