import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { DomainExpanded } from "@/lib/types";

const updateDomain = vi.fn();

vi.mock("@/lib/hooks/use-domains", () => ({
  useAddDomain: () => ({ mutateAsync: vi.fn(), isPending: false }),
  useUpdateDomain: () => ({ mutateAsync: updateDomain, isPending: false }),
}));
vi.mock("@/lib/hooks/use-certificates", () => ({
  useCertificates: () => ({ data: [], isLoading: false }),
  useUsableCertificates: () => ({ data: [], isLoading: false }),
}));
vi.mock("@/lib/hooks/use-dns-providers", () => ({
  useDNSProviders: () => ({ data: [], isLoading: false }),
}));
vi.mock("@/lib/stores/auth", () => ({
  useAuthStore: (sel: (s: { user: { role: string } }) => unknown) =>
    sel({ user: { role: "admin" } }),
}));
vi.mock("sonner", () => ({
  toast: {
    promise: (p: Promise<unknown>) => void p.catch(() => undefined),
    error: vi.fn(),
  },
}));

import { DomainFormDialog } from "./domain-form-dialog";
import { ForceHTTPSCell } from "./domains-table";

const offDomain = {
  id: "d1",
  hostname: "plain.example.com",
  path: "/",
  strip_path: false,
  internal_path: "",
  ssl_mode: "off",
  ssl_enabled: false,
  // The stale value the form used to persist for an ssl_mode=off domain.
  force_https: true,
} as DomainExpanded;

/**
 * jsdom asserts what is RENDERED as text and what is SUBMITTED. It cannot say
 * whether an operator can see the badge — layout and compositing are the browser
 * drill's job, not covered here.
 */
describe("force_https on an ssl_mode=off domain", () => {
  beforeEach(() => updateDomain.mockReset().mockResolvedValue({}));

  it("shows a stored force_https=true as HTTP only", () => {
    render(<ForceHTTPSCell domain={offDomain} />);
    expect(screen.getByText("HTTP only")).toBeTruthy();
    expect(screen.queryByText("Forced")).toBeNull();
  });

  it("still shows Forced when TLS is on", () => {
    render(<ForceHTTPSCell domain={{ ...offDomain, ssl_mode: "automatic" }} />);
    expect(screen.getByText("Forced")).toBeTruthy();
  });

  it("submits false, not the stale true the checkbox was hiding", async () => {
    render(
      <DomainFormDialog
        projectId="p"
        applicationId="a"
        domain={offDomain}
        open
        onOpenChange={() => undefined}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Save Changes/ }));
    await waitFor(() => expect(updateDomain).toHaveBeenCalled());
    expect(updateDomain.mock.calls[0][0]).toMatchObject({
      domainId: "d1",
      ssl_mode: "off",
      force_https: false,
    });
  });
});
