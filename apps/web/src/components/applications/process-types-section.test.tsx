import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import type { ProcessType } from "@/lib/types";

const createMutate = vi.fn();
const deleteMutate = vi.fn();
const restartMutate = vi.fn();
const reloadMutate = vi.fn();
let admin = true;
const updateMutate = vi.fn();
const toastWarning = vi.fn();
let types: ProcessType[] = [];
let volumes: unknown[] = [];
let appLimits = { cpu_limit: 0, memory_limit: 0 };

vi.mock("@/lib/hooks/use-process-types", () => ({
  useProcessTypes: () => ({ data: types, isLoading: false }),
  useCreateProcessType: () => ({
    mutateAsync: createMutate,
    isPending: false,
  }),
  useUpdateProcessType: () => ({ mutateAsync: updateMutate, isPending: false }),
  useDeleteProcessType: () => ({ mutateAsync: deleteMutate, isPending: false }),
  useRestartInstance: () => ({ mutateAsync: restartMutate, isPending: false }),
  useReloadInstance: () => ({ mutateAsync: reloadMutate, isPending: false }),
}));
vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    params,
    search,
    ...rest
  }: {
    children: React.ReactNode;
    to: string;
    params?: Record<string, string>;
    search?: Record<string, string>;
  }) => {
    const path = to.replace(/\$(\w+)/g, (_, k: string) => params?.[k] ?? "");
    const qs = search ? "?" + new URLSearchParams(search).toString() : "";
    return (
      <a href={path + qs} {...rest}>
        {children}
      </a>
    );
  },
}));
vi.mock("@/lib/stores/auth", () => ({
  useAuthStore: (sel: (s: { user: { role: string } }) => unknown) =>
    sel({ user: { role: admin ? "admin" : "member" } }),
}));
vi.mock("@/lib/hooks/use-volumes", () => ({
  useVolumes: () => ({ data: volumes }),
}));
vi.mock("@/lib/hooks/use-applications", () => ({
  useApplication: () => ({ data: appLimits }),
}));
vi.mock("sonner", () => ({
  toast: {
    promise: vi.fn(),
    warning: (m: string) => toastWarning(m),
    error: vi.fn(),
  },
}));

import { ProcessTypesSection } from "./process-types-section";

const pt = (over: Partial<ProcessType>): ProcessType => ({
  id: "id",
  name: "web",
  command: "",
  routable: false,
  replicas: 1,
  cpu_limit: null,
  memory_limit: null,
  health_check_type: null,
  health_check_command: null,
  // What the server resolves for a type with nothing set: the routable one inherits
  // http, a worker gets none. The component renders this; it does not recompute it.
  resolved_health: over.routable
    ? { kind: "http", source: "inherited", inert: false }
    : { kind: "none", source: "default", inert: false },
  instances: [],
  created_at: "",
  updated_at: "",
  ...over,
});

const renderIt = () =>
  render(<ProcessTypesSection projectId="p" applicationId="a" />);

/** Opens a kebab menu by its accessible name, and returns the menu's item names. */
async function openMenu(name: string) {
  fireEvent.click(screen.getByRole("button", { name }));
  await screen.findAllByRole("menuitem");
  return screen.getAllByRole("menuitem").map((i) => i.textContent?.trim());
}
async function clickItem(menu: string, item: string) {
  await openMenu(menu);
  fireEvent.click(screen.getByRole("menuitem", { name: item }));
}

/**
 * Rendered text and the accessible tree only — jsdom has no layout engine, so
 * whether a person can SEE any of this is the browser drill's job.
 */
describe("ProcessTypesSection", () => {
  beforeEach(() => {
    admin = true;
    reloadMutate.mockReset().mockResolvedValue({ status: "reloading" });
    volumes = [];
    appLimits = { cpu_limit: 0, memory_limit: 0 };
    createMutate.mockReset().mockResolvedValue({});
    deleteMutate.mockReset().mockResolvedValue({ status: "deleted" });
    toastWarning.mockReset();
    types = [
      pt({
        id: "w",
        name: "web",
        routable: true,
        instances: [
          {
            id: "i1",
            idx: 1,
            container_name: "proj-app-1234abcd",
            container_id: "c1",
            status: "running",
          },
        ],
      }),
      pt({
        id: "k",
        name: "worker",
        command: "bundle exec sidekiq",
        instances: [
          {
            id: "i2",
            idx: 1,
            container_name: "proj-app-1234abcd-worker-1",
            container_id: null,
            status: "pending",
          },
        ],
      }),
    ];
  });

  it("offers Delete on a worker's menu and not on the routable type's", async () => {
    renderIt();
    expect(await openMenu("Actions for worker")).toEqual(["Edit", "Delete"]);
    fireEvent.keyDown(document.activeElement ?? document.body, {
      key: "Escape",
    });
    expect(screen.getByText(/cannot be removed/i)).toBeTruthy();
  });

  it("puts the routable type alone at the top and the workers in their own section", () => {
    renderIt();
    const routableSection = screen.getByRole("region", { name: "Routable" });
    const workersSection = screen.getByRole("region", { name: "Workers" });
    expect(within(routableSection).getByText("web")).toBeTruthy();
    expect(within(routableSection).queryByText("worker")).toBeNull();
    expect(within(workersSection).getByText("worker")).toBeTruthy();
    expect(within(workersSection).queryByText("web")).toBeNull();
    // Routable first in the document.
    expect(
      routableSection.compareDocumentPosition(workersSection) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  // Both sections are lists of cards, so every card is a listitem whichever type it is
  // (a card whose element depended on its type broke three e2e specs).
  it("renders every card as a list item, in both sections", () => {
    renderIt();
    expect(
      within(screen.getByRole("region", { name: "Routable" })).getAllByRole(
        "listitem",
      ),
    ).toHaveLength(1);
    expect(
      within(screen.getByRole("region", { name: "Workers" })).getAllByRole(
        "listitem",
      ),
    ).toHaveLength(1);
  });

  it("names the type by its real name with a Routable badge, never a generic Main", () => {
    renderIt();
    const card = screen.getAllByTestId("type-card")[0];
    expect(within(card).getByText("web")).toBeTruthy();
    expect(within(card).getByText("Routable")).toBeTruthy();
    expect(screen.queryByText("Main")).toBeNull();
  });

  // G3's Metrics table and G5's project panel own per-container usage, and the form owns
  // limits: a third place to read them is a third place to disagree.
  it("shows no CPU, memory or limit indicator on a card", () => {
    types = [
      pt({
        id: "k",
        name: "worker",
        cpu_limit: 0.5,
        memory_limit: 256 * 1024 * 1024,
      }),
    ];
    renderIt();
    const card = screen.getByTestId("type-card");
    expect(within(card).queryByText(/CPU/i)).toBeNull();
    expect(within(card).queryByText(/Memory/i)).toBeNull();
    expect(within(card).queryByText(/inherit/i)).toBeNull();
    expect(within(card).queryByText(/limit/i)).toBeNull();
  });

  it("says a second HTTP process can never receive a domain", () => {
    renderIt();
    expect(screen.getByText(/can never receive a domain/i)).toBeTruthy();
    expect(screen.getByText(/two applications/i)).toBeTruthy();
  });

  it("shows the resolved health check, with a worker's as none", () => {
    renderIt();
    // Neither type chose its check, so both read "Default" on the card; the resolved
    // detail (the application's http for web, none for a worker) is the tooltip's.
    expect(screen.getAllByText("Default")).toHaveLength(2);
  });

  it("shows each type's container and status", () => {
    renderIt();
    // Shortened to {type}-{idx}; the full name is the row's title.
    expect(screen.getByText("worker-1")).toBeTruthy();
    expect(screen.getByTitle("proj-app-1234abcd-worker-1")).toBeTruthy();
    // A WORD, not just a colour: unhealthy, stopped and error are different facts.
    expect(screen.getByText("Pending")).toBeTruthy();
    expect(screen.getByText("Running")).toBeTruthy();
  });

  it("says when a change takes effect", () => {
    renderIt();
    expect(screen.getByText(/next deploy/i)).toBeTruthy();
    expect(screen.getByText(/immediately/i)).toBeTruthy();
  });

  it("offers replicas and per-type limits when adding a type", async () => {
    renderIt();
    fireEvent.click(screen.getByRole("button", { name: /Add Process Type/ }));
    expect(await screen.findByLabelText("Replicas")).toBeTruthy();
    expect(screen.getByLabelText("CPU (cores)")).toBeTruthy();
    expect(screen.getByLabelText("Memory (MB)")).toBeTruthy();
  });

  it("does not submit an invalid name, and does not flag it while typing", async () => {
    renderIt();
    fireEvent.click(screen.getByRole("button", { name: /Add Process Type/ }));
    const name = await screen.findByLabelText("Name");

    // Mid-typing a name that is not valid yet: no error before blur.
    fireEvent.change(name, { target: { value: "Bad_Name" } });
    expect(
      screen.queryByText(/starting with a letter/i, {
        selector: "[data-slot]",
      }),
    ).toBeNull();

    const submit = screen
      .getAllByRole("button", { name: "Add Process Type" })
      .at(-1)!;
    fireEvent.click(submit);
    await waitFor(() =>
      expect(screen.getByText(/starting with a letter/i)).toBeTruthy(),
    );
    expect(createMutate).not.toHaveBeenCalled();
  });

  it("creates a type with a valid name", async () => {
    renderIt();
    fireEvent.click(screen.getByRole("button", { name: /Add Process Type/ }));
    fireEvent.change(await screen.findByLabelText("Name"), {
      target: { value: "scheduler" },
    });
    const submit = screen
      .getAllByRole("button", { name: "Add Process Type" })
      .at(-1)!;
    fireEvent.click(submit);
    await waitFor(() => expect(createMutate).toHaveBeenCalledTimes(1));
    expect(createMutate.mock.calls[0][0]).toMatchObject({
      name: "scheduler",
      command: "",
      health_check_type: "",
      health_check_command: "",
    });
  });

  // The removal can fail after the row is gone, and the server reports that as a
  // warning on a 200. It must reach the operator: a container left running while the
  // row says it is gone is exactly what this exists to surface.
  it("surfaces a removal warning instead of swallowing it", async () => {
    deleteMutate.mockResolvedValue({
      status: "deleted",
      warning: "its container could not be removed yet",
    });
    renderIt();
    await clickItem("Actions for worker", "Delete");
    fireEvent.click(
      await screen.findByRole("button", { name: "Remove process type" }),
    );
    await waitFor(() =>
      expect(toastWarning).toHaveBeenCalledWith(
        "its container could not be removed yet",
      ),
    );
    expect(deleteMutate).toHaveBeenCalledWith("k");
  });

  // The component renders what the SERVER resolved; it does not recompute the rule.
  // A deliberately inconsistent fixture proves it: this "worker" is non-routable with
  // no type of its own, which the old client copy would have shown as "None (default)".
  it("renders the health check from the API, not from a local copy of the rule", () => {
    types = [
      pt({
        id: "k",
        name: "worker",
        resolved_health: { kind: "command", source: "set", inert: false },
        health_check_command: "pgrep worker",
      }),
    ];
    renderIt();
    // The kind it chose is the label; the exact command is the tooltip's.
    expect(screen.getByText("Command")).toBeTruthy();
    expect(screen.queryByText("None")).toBeNull();
    expect(screen.queryByText("Default")).toBeNull();
  });

  // The server says nothing is probed (http, no path). The row must say so in words a
  // person cannot misread as protection, and keep the chosen kind visible.
  it("warns when the health check resolves to http with no path", () => {
    types = [
      pt({
        id: "w",
        resolved_health: {
          kind: "http",
          source: "inherited",
          inert: true,
          reason: "no health check path is set, so nothing is probed",
        },
      }),
    ];
    renderIt();
    const warn = screen.getByTestId("health-inert");
    // The card is short: the amber icon and "HTTP probe". The sentence is the tooltip's,
    // with a screen-reader copy so it is not lost to someone who cannot hover.
    const label = within(warn).getByText("HTTP probe", { selector: "span" });
    expect(label.className).toMatch(/amber/);
    expect(warn.querySelector("svg")).not.toBeNull();
    const sr = warn.querySelector(".sr-only");
    expect(sr?.textContent).toContain("no path set, so nothing is checked");
    expect(sr?.textContent).toContain("without checking the new one");
    // ⚠️ jsdom has no layout engine: this proves the label, icon and text are in the document,
    // not that they render (toBeVisible() passes on an opacity: 0 block) or that the tooltip
    // opens. Only a browser does. A tooltip is also unreachable on touch: the amber icon and
    // label are what a touch user sees.
  });

  it("shows the plain label, no warning, for a probe that can run", () => {
    types = [pt({ id: "ok", routable: true })];
    renderIt();
    expect(screen.queryByTestId("health-inert")).toBeNull();
    expect(screen.getByText("Default")).toBeTruthy();
  });

  it("restarts exactly the instance whose control was used", async () => {
    restartMutate.mockResolvedValue({ status: "restarted" });
    types = [
      pt({
        id: "w",
        name: "web",
        routable: true,
        instances: [
          {
            id: "i-web",
            idx: 1,
            container_name: "app",
            container_id: "c1",
            status: "running",
          },
        ],
      }),
      pt({
        id: "k",
        name: "worker",
        instances: [
          {
            id: "i-worker",
            idx: 1,
            container_name: "app-worker-1",
            container_id: "c2",
            status: "running",
          },
        ],
      }),
    ];
    renderIt();
    await clickItem("Actions for app-worker-1", "Restart");
    await waitFor(() => expect(restartMutate).toHaveBeenCalledTimes(1));
    expect(restartMutate).toHaveBeenCalledWith("i-worker");
  });

  it("offers no restart, reload or logs for an instance that has no container yet", async () => {
    renderIt();
    // The fixture's worker is 'pending' with container_id null: nothing to act on.
    const items = await openMenu("Actions for proj-app-1234abcd-worker-1");
    expect(items).not.toContain("View Logs");
    expect(
      screen
        .getByRole("menuitem", { name: "Restart" })
        .getAttribute("aria-disabled"),
    ).toBe("true");
    expect(
      screen
        .getByRole("menuitem", { name: "Reload" })
        .getAttribute("aria-disabled"),
    ).toBe("true");
  });

  // The routable type cannot be paused. The operator is told in their own terms
  // before the round trip; the server refuses the same thing and stays the authority.
  it("refuses 0 replicas on the web process, and offers a pause on a worker", async () => {
    renderIt();
    await clickItem("Actions for web", "Edit");
    const replicas = await screen.findByLabelText("Replicas");
    fireEvent.change(replicas, { target: { value: "0" } });
    fireEvent.blur(replicas);
    await waitFor(() =>
      expect(screen.getByText(/cannot be paused/i)).toBeTruthy(),
    );
  });

  it("labels 0 as a pause on a worker", async () => {
    renderIt();
    await clickItem("Actions for worker", "Edit");
    fireEvent.change(await screen.findByLabelText("Replicas"), {
      target: { value: "0" },
    });
    await waitFor(() => expect(screen.getByText(/Paused\./)).toBeTruthy());
  });

  it("refuses more than one replica of any type when the application has a volume", async () => {
    volumes = [{ id: "v" }];
    renderIt();
    await clickItem("Actions for worker", "Edit");
    const replicas = await screen.findByLabelText("Replicas");
    fireEvent.change(replicas, { target: { value: "2" } });
    fireEvent.blur(replicas);
    await waitFor(() =>
      expect(screen.getByText(/persistent volumes/i)).toBeTruthy(),
    );
  });

  // A blank limit is not "zero": it inherits the application's, and says so.
  it("shows what an empty limit inherits instead of a bare blank", async () => {
    appLimits = { cpu_limit: 2, memory_limit: 512 * 1024 * 1024 };
    renderIt();
    await clickItem("Actions for worker", "Edit");
    expect(
      await screen.findByPlaceholderText("Inherit — 2 cores"),
    ).toBeTruthy();
    expect(screen.getByPlaceholderText("Inherit — 512 MB")).toBeTruthy();
  });

  it("sends replicas and clears blank limits when saving", async () => {
    updateMutate.mockResolvedValue({});
    renderIt();
    await clickItem("Actions for worker", "Edit");
    fireEvent.change(await screen.findByLabelText("Replicas"), {
      target: { value: "3" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(updateMutate).toHaveBeenCalledTimes(1));
    expect(updateMutate.mock.calls[0][0].data).toMatchObject({
      replicas: 3,
      cpu_limit: 0,
      memory_limit: 0,
    });
  });

  it("shows a paused type as paused, with no containers", () => {
    types = [pt({ id: "k", name: "worker", replicas: 0, instances: [] })];
    renderIt();
    expect(screen.getByText("0 — paused")).toBeTruthy();
    expect(screen.getByText("Paused — no containers")).toBeTruthy();
  });

  // The organising rule: nothing in a menu may contradict Replicas. Stop (and Start) would
  // leave a card reading "Replicas 3" over two containers, because nothing enforces replicas
  // between deploys. Replicas 0 is the durable off.
  it("offers no Stop and no Start anywhere", async () => {
    renderIt();
    for (const menu of [
      "Actions for web",
      "Actions for worker",
      "Actions for proj-app-1234abcd",
      "Actions for proj-app-1234abcd-worker-1",
    ]) {
      const items = await openMenu(menu);
      expect(items.join("|")).not.toMatch(/\b(stop|start)\b/i);
      fireEvent.keyDown(document.activeElement ?? document.body, {
        key: "Escape",
      });
      await waitFor(() =>
        expect(screen.queryAllByRole("menuitem")).toHaveLength(0),
      );
    }
    expect(
      screen.queryByRole("button", { name: /^(stop|start)\b/i }),
    ).toBeNull();
  });

  it("gives an instance View Logs, Inspect (admin), Restart and Reload", async () => {
    renderIt();
    expect(await openMenu("Actions for proj-app-1234abcd")).toEqual([
      "View Logs",
      "Inspect",
      "Restart",
      "Reload",
    ]);
    // View Logs deep-links to that container's id, which is what the viewer's session
    // state is keyed on.
    expect(
      screen.getByRole("menuitem", { name: "View Logs" }).getAttribute("href"),
    ).toBe("/projects/p/applications/a/logs?session=c1");
    expect(
      screen.getByRole("menuitem", { name: "Inspect" }).getAttribute("href"),
    ).toBe("/docker?tab=containers&q=proj-app-1234abcd");
  });

  it("never renders Inspect for a non-admin: the Docker page 403s for them", async () => {
    admin = false;
    renderIt();
    const items = await openMenu("Actions for proj-app-1234abcd");
    expect(items).not.toContain("Inspect");
    expect(items).toContain("View Logs");
  });

  describe("long instance lists", () => {
    const many = (n: number) =>
      Array.from({ length: n }, (_, i) => ({
        id: `i${i}`,
        idx: i + 1,
        container_name: `w-${i + 1}`,
        container_id: `c${i}`,
        status: "running",
      }));

    it("collapses behind 'N more' and expands, and collapses again", () => {
      types = [
        pt({ id: "k", name: "worker", replicas: 7, instances: many(7) }),
      ];
      renderIt();
      expect(screen.getByText("worker-3")).toBeTruthy();
      expect(screen.queryByText("worker-4")).toBeNull();
      const more = screen.getByRole("button", { name: "4 more" });
      expect(more.getAttribute("aria-expanded")).toBe("false");
      fireEvent.click(more);
      expect(screen.getByText("worker-7")).toBeTruthy();
      const fewer = screen.getByRole("button", { name: "Show fewer" });
      expect(fewer.getAttribute("aria-expanded")).toBe("true");
      fireEvent.click(fewer);
      expect(screen.queryByText("worker-4")).toBeNull();
    });

    it("has no control when everything fits", () => {
      types = [
        pt({ id: "k", name: "worker", replicas: 3, instances: many(3) }),
      ];
      renderIt();
      expect(screen.queryByRole("button", { name: /more|fewer/i })).toBeNull();
    });
  });

  describe("Reload", () => {
    const web = (n: number) =>
      pt({
        id: "w",
        name: "web",
        routable: true,
        replicas: n,
        instances: Array.from({ length: n }, (_, i) => ({
          id: `iw${i}`,
          idx: i + 1,
          container_name: `web-${i + 1}`,
          container_id: `cw${i}`,
          status: "running",
        })),
      });

    it("leads with the outage for the only routable container, and offers the gap-free reload after it", async () => {
      types = [web(1)];
      renderIt();
      await clickItem("Actions for web-1", "Reload");
      const dialog = await screen.findByRole("alertdialog");
      const text = dialog.textContent ?? "";
      expect(text.indexOf("unavailable")).toBeGreaterThan(-1);
      expect(text.indexOf("unavailable")).toBeLessThan(
        text.indexOf("without a gap"),
      );
      expect(text).not.toMatch(/mixed config/i);
    });

    it("does not offer a gap-free reload when volumes rule out a roll", async () => {
      volumes = [{ id: "v" }];
      types = [web(1)];
      renderIt();
      await clickItem("Actions for web-1", "Reload");
      const text = (await screen.findByRole("alertdialog")).textContent ?? "";
      expect(text).toMatch(/unavailable/);
      expect(text).not.toMatch(/without a gap/);
    });

    it("warns about mixed config when one replica of several is reloaded", async () => {
      types = [web(3)];
      renderIt();
      await clickItem("Actions for web-2", "Reload");
      const text = (await screen.findByRole("alertdialog")).textContent ?? "";
      expect(text).toMatch(/mixed config/i);
      expect(text).toMatch(/other containers keep serving/i);
      expect(text).not.toMatch(/application will be unavailable/i);
    });

    it("never promises verification", async () => {
      types = [web(1)];
      renderIt();
      await clickItem("Actions for web-1", "Reload");
      const text = (await screen.findByRole("alertdialog")).textContent ?? "";
      expect(text).toMatch(/not health-checked/i);
      expect(text).not.toMatch(/verif|will be checked|safe/i);
    });

    it("reloads exactly the instance whose control was used, only after confirming", async () => {
      types = [web(3)];
      renderIt();
      await clickItem("Actions for web-2", "Reload");
      expect(reloadMutate).not.toHaveBeenCalled();
      fireEvent.click(await screen.findByRole("button", { name: "Reload" }));
      await waitFor(() => expect(reloadMutate).toHaveBeenCalledTimes(1));
      expect(reloadMutate).toHaveBeenCalledWith("iw1");
    });
  });
});
