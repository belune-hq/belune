import type { DomainExpanded, RouteFeature } from "@/lib/types";
import { api } from "./client";

export function listDomains(projectId: string, applicationId: string) {
  return api.get<DomainExpanded[]>(
    `/projects/${projectId}/applications/${applicationId}/domains`,
  );
}

export function addDomain(
  projectId: string,
  applicationId: string,
  data: {
    hostname: string;
    path?: string;
    strip_path?: boolean;
    internal_path?: string;
    ssl_enabled?: boolean;
    container_port?: number;
    force_https?: boolean;
    ssl_mode?: string;
    ssl_provider?: string;
    certificate_id?: string;
    /** Absent keeps the stored provider; "" detaches it. */
    dns_provider_id?: string;
  },
) {
  return api.post<DomainExpanded>(
    `/projects/${projectId}/applications/${applicationId}/domains`,
    data,
  );
}

export function updateDomain(
  projectId: string,
  applicationId: string,
  domainId: string,
  data: {
    hostname?: string;
    path?: string;
    strip_path?: boolean;
    internal_path?: string;
    container_port?: number | null;
    ssl_enabled?: boolean;
    force_https?: boolean;
    ssl_mode?: string;
    ssl_provider?: string;
    certificate_id?: string;
    /** Absent keeps the stored provider; "" detaches it. */
    dns_provider_id?: string;
  },
) {
  return api.put<DomainExpanded>(
    `/projects/${projectId}/applications/${applicationId}/domains/${domainId}`,
    data,
  );
}

export function removeDomain(
  projectId: string,
  applicationId: string,
  domainId: string,
) {
  return api.delete<void>(
    `/projects/${projectId}/applications/${applicationId}/domains/${domainId}`,
  );
}

export function listRouteFeatures(
  projectId: string,
  applicationId: string,
  domainId: string,
) {
  return api.get<RouteFeature[]>(
    `/projects/${projectId}/applications/${applicationId}/domains/${domainId}/features`,
  );
}

export function upsertRouteFeature(
  projectId: string,
  applicationId: string,
  domainId: string,
  data: {
    feature_type: string;
    config: Record<string, unknown>;
    enabled: boolean;
  },
) {
  return api.put<RouteFeature>(
    `/projects/${projectId}/applications/${applicationId}/domains/${domainId}/features`,
    data,
  );
}

export function deleteRouteFeature(
  projectId: string,
  applicationId: string,
  domainId: string,
  featureId: string,
) {
  return api.delete<void>(
    `/projects/${projectId}/applications/${applicationId}/domains/${domainId}/features/${featureId}`,
  );
}
