import type { TrustedProxies } from "@/lib/types";
import { api } from "./client";

export function getTrustedProxies() {
  return api.get<TrustedProxies>("/maintenance/proxy/trusted-proxies");
}

/**
 * Replaces the whole list; an empty one turns trusting off. The response is the
 * list as stored (canonical, sorted) with any overlap Belune can see.
 */
export function setTrustedProxies(cidrs: string[]) {
  return api.put<TrustedProxies>("/maintenance/proxy/trusted-proxies", {
    cidrs,
  });
}
