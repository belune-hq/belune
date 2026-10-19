import type { ProxyCapabilities } from "@/lib/types";
import { api } from "./client";

export function getProxyCapabilities() {
  return api.get<ProxyCapabilities>("/proxy/capabilities");
}
