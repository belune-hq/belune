import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { EditorView } from "@codemirror/view";
import { toast } from "sonner";
import type { ReactNode } from "react";
import type { DomainExpanded } from "@/lib/types";
import { DomainFeatures } from "@/components/domains/domain-features";
import { CONFIG_EXAMPLES } from "@/lib/route-feature-examples";

const mutateAsync = vi.hoisted(() => vi.fn());

vi.mock("sonner", () => ({
  toast: Object.assign(vi.fn(), { error: vi.fn(), promise: vi.fn() }),
}));
vi.mock("@/lib/hooks/use-proxy-capabilities", () => ({
  useProxyCapabilities: () => ({ data: undefined }),
}));
vi.mock("@/lib/hooks/use-domains", () => ({
  useDomains: () => ({ data: undefined }),
  useUpsertRouteFeature: () => ({ mutateAsync, isPending: false }),
  useDeleteRouteFeature: () => ({ mutateAsync: vi.fn(), isPending: false }),
}));
// base-ui's Select needs real pointer and layout behaviour to open, which jsdom
// does not have. A native <select> drives the same onValueChange.
vi.mock("@/components/ui/select", () => ({
  Select: ({
    value,
    onValueChange,
    children,
  }: {
    value: string;
    onValueChange: (v: string) => void;
    children: ReactNode;
  }) => (
    <select
      aria-label="Type"
      value={value}
      onChange={(e) => onValueChange(e.target.value)}
    >
      <option value="" />
      {children}
    </select>
  ),
  SelectTrigger: () => null,
  SelectValue: () => null,
  SelectContent: ({ children }: { children: ReactNode }) => <>{children}</>,
  SelectItem: ({ value, children }: { value: string; children: ReactNode }) => (
    <option value={value}>{children}</option>
  ),
}));

beforeAll(() => {
  const rect = {
    x: 0,
    y: 0,
    top: 0,
    left: 0,
    right: 0,
    bottom: 0,
    width: 0,
    height: 0,
  };
  Range.prototype.getClientRects = () =>
    [] as unknown as ReturnType<Range["getClientRects"]>;
  Range.prototype.getBoundingClientRect = () => rect as DOMRect;
});

beforeEach(() => {
  vi.clearAllMocks();
  mutateAsync.mockResolvedValue({});
  // The component hands the mutation straight to toast.promise, so what is
  // asserted below is what was sent, not what the toast shows.
  vi.mocked(toast.promise).mockImplementation(
    ((p: Promise<unknown>) => p) as never,
  );
});

const domain = {
  id: "dom-1",
  hostname: "example.test",
  route_features: [],
} as unknown as DomainExpanded;

async function renderFeatures() {
  const utils = render(
    <DomainFeatures projectId="p" applicationId="a" domain={domain} />,
  );
  fireEvent.change(screen.getByLabelText("Type"), {
    target: { value: "rate_limit" },
  });
  // The editor arrives after its chunk loads; the field is usable before that.
  await waitFor(() =>
    expect(utils.container.querySelector(".cm-editor")).not.toBeNull(),
  );
  const dom = utils.container.querySelector<HTMLElement>(".cm-editor")!;
  return { ...utils, view: EditorView.findFromDOM(dom)! };
}

// act(): CodeMirror reports the edit from its own event handling, outside
// React's, so without it the click below would read a render that predates it.
function setConfig(view: EditorView, text: string) {
  act(() => {
    view.dispatch({
      changes: { from: 0, to: view.state.doc.length, insert: text },
    });
  });
}

describe("route feature config field", () => {
  it("prefills the rate-limit example when the type is picked", async () => {
    const { view } = await renderFeatures();
    expect(view.state.doc.toString()).toBe('{"window":"1s","max_events":10}');
  });

  it("sends what was typed, parsed, and nothing else", async () => {
    const { view } = await renderFeatures();
    setConfig(view, '{"window":"5s","max_events":3}');

    fireEvent.click(screen.getByRole("button", { name: "Add feature" }));

    expect(mutateAsync).toHaveBeenCalledExactlyOnceWith({
      domainId: "dom-1",
      feature_type: "rate_limit",
      config: { window: "5s", max_events: 3 },
      enabled: true,
    });
  });

  // The footgun, and the reason the editor must stay out of validation:
  // max_events 0 is valid JSON, makes a route answer nothing at all, and is
  // rejected by the SERVER (ParseFeatureConfig). The client must neither block
  // it nor rewrite it: the server's answer is what the person should see.
  it("lets max_events 0 through to the server unchanged", async () => {
    const { view } = await renderFeatures();
    setConfig(view, '{"window":"1s","max_events":0}');

    fireEvent.click(screen.getByRole("button", { name: "Add feature" }));

    expect(toast.error).not.toHaveBeenCalled();
    expect(mutateAsync).toHaveBeenCalledExactlyOnceWith(
      expect.objectContaining({ config: { window: "1s", max_events: 0 } }),
    );
  });

  it("still stops at JSON.parse for malformed JSON, with the existing message", async () => {
    const { view } = await renderFeatures();
    setConfig(view, '{"window":');

    fireEvent.click(screen.getByRole("button", { name: "Add feature" }));

    expect(toast.error).toHaveBeenCalledExactlyOnceWith("Invalid JSON config");
    expect(mutateAsync).not.toHaveBeenCalled();
  });
});

function pick(type: string) {
  fireEvent.change(screen.getByLabelText("Type"), { target: { value: type } });
}

describe("config examples", () => {
  // The bug that only exists once every type has an example. Rate Limit's
  // example is not "{}", so a rule that only recognised "{}" would leave a
  // rate_limit config in the field under a Basic Auth label.
  it("swaps one untouched example for the next as the type changes", async () => {
    const { view } = await renderFeatures();
    expect(view.state.doc.toString()).toBe(CONFIG_EXAMPLES.rate_limit);

    pick("basic_auth");
    expect(view.state.doc.toString()).toBe(CONFIG_EXAMPLES.basic_auth);

    pick("redirect");
    expect(view.state.doc.toString()).toBe(CONFIG_EXAMPLES.redirect);
  });

  // The other half, and the one that matters more: silently discarding typed
  // work is far worse than leaving a stale example.
  it("never replaces something the person edited", async () => {
    const { view } = await renderFeatures();
    setConfig(view, '{"window":"5s","max_events":3}');

    pick("ip_allowlist");

    expect((screen.getByLabelText("Type") as HTMLSelectElement).value).toBe(
      "ip_allowlist",
    );
    expect(view.state.doc.toString()).toBe('{"window":"5s","max_events":3}');
  });

  it("fills a cleared field again when the type changes", async () => {
    const { view } = await renderFeatures();
    setConfig(view, "");

    pick("headers");

    expect(view.state.doc.toString()).toBe(CONFIG_EXAMPLES.headers);
  });

  // One or the other: a placeholder only renders in an empty field, an example
  // only fills a non-empty one.
  it("never shows an example and a placeholder together", async () => {
    const { view, container } = await renderFeatures();
    const placeholder = () => container.querySelector(".cm-placeholder");

    pick("basic_auth");
    expect(view.state.doc.toString()).toBe(CONFIG_EXAMPLES.basic_auth);
    expect(placeholder()).toBeNull();

    setConfig(view, "");
    expect(placeholder()?.textContent).toBe(CONFIG_EXAMPLES.basic_auth);
  });

  // basic_auth's example is EXPECTED to be rejected, by the server, on purpose
  // (features.go: "username and hashed_password are required"): a fake hash would
  // pass the non-empty check and then silently never authenticate. The client
  // sends it as it is and has no opinion; what the person sees is the server's
  // message. This is not a failing case.
  it("sends basic_auth's empty example as it is and shows the server's rejection", async () => {
    const reason = "basic_auth: username and hashed_password are required";
    mutateAsync.mockRejectedValueOnce(new Error(reason));
    vi.mocked(toast.promise).mockImplementation(((p: Promise<unknown>) => {
      p.catch(() => {}); // sonner would handle the rejection; the mock must too
      return p;
    }) as never);
    await renderFeatures();
    pick("basic_auth");

    fireEvent.click(screen.getByRole("button", { name: "Add feature" }));

    expect(toast.error).not.toHaveBeenCalled();
    expect(mutateAsync).toHaveBeenCalledExactlyOnceWith({
      domainId: "dom-1",
      feature_type: "basic_auth",
      config: { username: "", hashed_password: "" },
      enabled: true,
    });
    const { error } = vi.mocked(toast.promise).mock.calls[0][1] as {
      error: (e: Error) => string;
    };
    expect(error(new Error(reason))).toBe(reason);
  });

  it("says where basic_auth's hash comes from, for basic_auth only", async () => {
    const { container } = await renderFeatures();
    expect(container.textContent).not.toContain("caddy hash-password");

    pick("basic_auth");
    expect(container.textContent).toContain("caddy hash-password");
    // Tied to the field as its description, not just placed near it.
    const field = screen.getByRole("textbox", { name: "Config (JSON)" });
    const hint = document.getElementById(
      field.getAttribute("aria-describedby") ?? "",
    );
    expect(hint?.textContent).toContain("bcrypt hash");

    pick("rate_limit");
    expect(container.textContent).not.toContain("caddy hash-password");
    expect(
      screen
        .getByRole("textbox", { name: "Config (JSON)" })
        .hasAttribute("aria-describedby"),
    ).toBe(false);
  });
});
