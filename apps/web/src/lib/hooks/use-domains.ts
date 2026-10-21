import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { queryKeys } from "./query-keys";
import * as domainsApi from "@/lib/api/domains";

export function useDomains(projectId: string, applicationId: string) {
  return useQuery({
    queryKey: queryKeys.domains.all(projectId, applicationId),
    queryFn: () => domainsApi.listDomains(projectId, applicationId),
    // Poll while any domain's TLS is still settling.
    //
    // Adding a domain invalidates this query immediately, which reads the row back
    // before the probe the server just enqueued has had time to run — so the badge
    // showed "Checking…" and, with nothing ever refetching, stayed there until the
    // operator reloaded the page by hand. The status was right in the database
    // within a second; the UI simply never asked again.
    //
    // Two speeds, because the two states resolve on different timescales. `unknown`
    // is the freshly-created row waiting on that one-shot probe: it lasts seconds,
    // and the operator is watching. `pending` means a certificate is still being
    // obtained, which can legitimately take minutes — or sit there indefinitely
    // when ACME is failing — so polling that every three seconds would hammer the
    // API for the entire time a domain is broken.
    refetchInterval: (query) => {
      const domains = query.state.data;
      if (!domains?.length) return false;

      const statuses = domains.map((d) => d.tls_status ?? "unknown");
      if (statuses.includes("unknown")) return 3000;
      if (statuses.includes("pending")) return 15000;
      return false;
    },
  });
}

export function useAddDomain(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: Parameters<typeof domainsApi.addDomain>[2]) =>
      domainsApi.addDomain(projectId, applicationId, data),
    onSuccess: () =>
      qc.invalidateQueries({
        queryKey: queryKeys.domains.all(projectId, applicationId),
      }),
  });
}

export function useUpdateDomain(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      domainId,
      ...data
    }: { domainId: string } & Parameters<typeof domainsApi.updateDomain>[3]) =>
      domainsApi.updateDomain(projectId, applicationId, domainId, data),
    onSuccess: () =>
      qc.invalidateQueries({
        queryKey: queryKeys.domains.all(projectId, applicationId),
      }),
  });
}

export function useRemoveDomain(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (domainId: string) =>
      domainsApi.removeDomain(projectId, applicationId, domainId),
    onSuccess: () =>
      qc.invalidateQueries({
        queryKey: queryKeys.domains.all(projectId, applicationId),
      }),
  });
}

export function useRouteFeatures(
  projectId: string,
  applicationId: string,
  domainId: string,
) {
  return useQuery({
    queryKey: queryKeys.routeFeatures(projectId, applicationId, domainId),
    queryFn: () =>
      domainsApi.listRouteFeatures(projectId, applicationId, domainId),
    enabled: !!domainId,
  });
}

export function useUpsertRouteFeature(
  projectId: string,
  applicationId: string,
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      domainId,
      ...data
    }: {
      domainId: string;
      feature_type: string;
      config: Record<string, unknown>;
      enabled: boolean;
    }) => domainsApi.upsertRouteFeature(projectId, applicationId, domainId, data),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.domains.all(projectId, applicationId),
      });
    },
  });
}

export function useDeleteRouteFeature(
  projectId: string,
  applicationId: string,
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      domainId,
      featureId,
    }: {
      domainId: string;
      featureId: string;
    }) => domainsApi.deleteRouteFeature(projectId, applicationId, domainId, featureId),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.domains.all(projectId, applicationId),
      });
    },
  });
}
