import { beforeEach, describe, expect, it, vi } from "vitest";
import { render } from "@testing-library/react";

const useLogs = vi.fn();

vi.mock("@/lib/hooks/use-container-logs", () => ({
  useContainerLogs: (...args: unknown[]) => {
    useLogs(...args);
    return { data: [], isLoading: false, error: null };
  },
  useContainerLogSessions: () => ({
    data: [
      {
        container_id: "c-web",
        deployment_id: "d1",
        process_type: "web",
        instance_idx: 0,
        first_at: "2026-11-16T00:00:00Z",
        last_at: "2026-11-16T00:00:00Z",
        line_count: 1,
        triggered_by: null,
        status: null,
        commit_sha: null,
        started_at: null,
      },
      {
        container_id: "c-worker",
        deployment_id: "d1",
        process_type: "worker",
        instance_idx: 0,
        first_at: "2026-11-16T00:00:00Z",
        last_at: "2026-11-16T00:00:00Z",
        line_count: 1,
        triggered_by: null,
        status: null,
        commit_sha: null,
        started_at: null,
      },
    ],
  }),
}));
vi.mock("@/lib/hooks/use-websocket", () => ({
  useChannel: () => ({ connected: false }),
}));

import { ContainerLogViewer } from "./container-log-viewer";

const lastQuery = () =>
  useLogs.mock.calls.at(-1)![3] as {
    session?: string;
    processType?: string;
  };

describe("ContainerLogViewer deep link", () => {
  beforeEach(() => useLogs.mockReset());

  it("opens on the routable type by default", () => {
    render(
      <ContainerLogViewer
        source="application"
        projectId="p"
        sourceId="a"
        defaultProcessType="web"
      />,
    );
    expect(lastQuery().processType).toBe("web");
    expect(lastQuery().session).toBeUndefined();
  });

  // A "View Logs" link on a WORKER's container must not land on a pane filtered to web,
  // which would show nothing: the session is selected and the type filter is lifted.
  it("opens on the linked container with the type filter lifted", () => {
    render(
      <ContainerLogViewer
        source="application"
        projectId="p"
        sourceId="a"
        defaultProcessType="web"
        initialSession="c-worker"
      />,
    );
    expect(lastQuery().session).toBe("c-worker");
    expect(lastQuery().processType).toBeUndefined();
  });
});
