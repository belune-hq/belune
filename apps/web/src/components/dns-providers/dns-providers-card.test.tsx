import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { DNSProvider } from "@/lib/api/dns-providers";

const create = vi.fn();
const update = vi.fn();
let providers: DNSProvider[] = [];

vi.mock("@/lib/hooks/use-dns-providers", () => ({
  useDNSProviders: () => ({ data: providers, isLoading: false }),
  useCreateDNSProvider: () => ({ mutateAsync: create, isPending: false }),
  useUpdateDNSProvider: () => ({ mutateAsync: update, isPending: false }),
  useDeleteDNSProvider: () => ({ mutateAsync: vi.fn(), isPending: false }),
}));

import { DNSProvidersCard } from "./dns-providers-card";

const existing: DNSProvider = {
  id: "p1",
  name: "cf",
  provider: "cloudflare",
  domain_count: 0,
  created_at: "",
  updated_at: "",
};

/** Rendered text and values only — layout is the browser drill's job. */
describe("DNSProvidersCard", () => {
  beforeEach(() => {
    create.mockReset().mockResolvedValue({});
    update.mockReset().mockResolvedValue({});
    providers = [];
  });

  it("sends only the selected provider's filled credential fields", async () => {
    render(<DNSProvidersCard />);
    fireEvent.click(screen.getByRole("button", { name: "Add Provider" }));
    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: "  Prod  " },
    });
    fireEvent.change(screen.getByLabelText(/API token/), {
      target: { value: " tok " },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add Provider" }));

    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(create).toHaveBeenCalledWith({
      name: "Prod",
      provider: "cloudflare",
      credentials: { api_token: "tok" },
    });
  });

  it("does not flag an untouched required field until a submit is attempted", async () => {
    render(<DNSProvidersCard />);
    fireEvent.click(screen.getByRole("button", { name: "Add Provider" }));
    expect(screen.queryByText("API token is required")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Add Provider" }));
    await waitFor(() =>
      expect(screen.getByText("API token is required")).toBeTruthy(),
    );
    expect(create).not.toHaveBeenCalled();
  });

  it("keeps the stored credentials when an edit leaves them blank", async () => {
    providers = [existing];
    render(<DNSProvidersCard />);
    fireEvent.click(screen.getByRole("button", { name: "Edit cf" }));
    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: "renamed" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(update).toHaveBeenCalled());
    expect(update).toHaveBeenCalledWith({
      id: "p1",
      name: "renamed",
      credentials: undefined,
    });
  });

  it("never renders a secret input as plain text", () => {
    render(<DNSProvidersCard />);
    fireEvent.click(screen.getByRole("button", { name: "Add Provider" }));
    expect((screen.getByLabelText(/API token/) as HTMLInputElement).type).toBe(
      "password",
    );
  });
});
