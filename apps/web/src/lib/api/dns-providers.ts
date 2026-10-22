import { api } from "./client";

export type DNSProviderType = "cloudflare" | "route53" | "digitalocean";

/** A stored provider. The credentials are write-only: the API never returns
 *  them, so there is nothing here to render or leak. */
export interface DNSProvider {
  id: string;
  name: string;
  provider: DNSProviderType;
  domain_count: number;
  created_at: string;
  updated_at: string;
}

export interface SaveDNSProvider {
  name: string;
  provider?: DNSProviderType;
  /** Keyed by the provider's own field names. On update, absent keeps the
   *  stored secret; present replaces it wholesale. */
  credentials?: Record<string, string>;
}

export function listDNSProviders() {
  return api.get<DNSProvider[]>("/dns-providers");
}

export function createDNSProvider(data: SaveDNSProvider) {
  return api.post<DNSProvider>("/dns-providers", data);
}

export function updateDNSProvider(id: string, data: SaveDNSProvider) {
  return api.put<DNSProvider>(`/dns-providers/${id}`, data);
}

export function deleteDNSProvider(id: string) {
  return api.delete<void>(`/dns-providers/${id}`);
}
