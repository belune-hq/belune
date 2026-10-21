import { useQuery, useMutation } from "@tanstack/react-query";
import { queryKeys } from "./query-keys";
import * as templatesApi from "@/lib/api/templates";
import type { InstantiateTemplateRequest } from "@/lib/api/templates";

export function useTemplates() {
  return useQuery({
    queryKey: queryKeys.templates.all,
    queryFn: templatesApi.listTemplates,
    // The catalog is embedded in the binary — effectively static per build.
    staleTime: 5 * 60 * 1000,
  });
}

export function useTemplate(id: string, enabled = true) {
  return useQuery({
    queryKey: queryKeys.templates.detail(id),
    queryFn: () => templatesApi.getTemplate(id),
    enabled: enabled && !!id,
    staleTime: 5 * 60 * 1000,
  });
}

export function useInstantiateTemplate() {
  return useMutation({
    mutationFn: ({ id, data }: { id: string; data: InstantiateTemplateRequest }) =>
      templatesApi.instantiateTemplate(id, data),
  });
}
