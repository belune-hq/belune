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
