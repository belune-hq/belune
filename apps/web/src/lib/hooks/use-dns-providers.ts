import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { queryKeys } from "./query-keys";
import * as dnsApi from "@/lib/api/dns-providers";
import type { SaveDNSProvider } from "@/lib/api/dns-providers";

// Admin-only at the API, so callers pass enabled=false for anyone else rather
// than firing a request that can only 403.
export function useDNSProviders(enabled = true) {
  return useQuery({
    queryKey: queryKeys.dnsProviders,
    queryFn: dnsApi.listDNSProviders,
    enabled,
  });
}

export function useCreateDNSProvider() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: SaveDNSProvider) => dnsApi.createDNSProvider(data),
    onSuccess: () => {
      toast.success("DNS provider added");
      qc.invalidateQueries({ queryKey: queryKeys.dnsProviders });
    },
    onError: (err) => toast.error(err.message),
  });
}

export function useUpdateDNSProvider() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...data }: SaveDNSProvider & { id: string }) =>
      dnsApi.updateDNSProvider(id, data),
    onSuccess: (_res, vars) => {
      toast.success(
        vars.credentials ? "Credentials rotated" : "DNS provider updated",
      );
      qc.invalidateQueries({ queryKey: queryKeys.dnsProviders });
    },
    onError: (err) => toast.error(err.message),
  });
}

export function useDeleteDNSProvider() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => dnsApi.deleteDNSProvider(id),
    onSuccess: () => {
      toast.success("DNS provider deleted");
      qc.invalidateQueries({ queryKey: queryKeys.dnsProviders });
    },
    // A 409 names how many domains still use it.
    onError: (err) => toast.error(err.message),
  });
}
