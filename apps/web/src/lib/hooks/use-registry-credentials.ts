import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  listRegistryCredentials,
  createRegistryCredential,
  updateRegistryCredential,
  deleteRegistryCredential,
  type RegistryCredentialInput,
} from "@/lib/api/registry-credentials";
import { queryKeys } from "./query-keys";

export function useRegistryCredentials() {
  return useQuery({
    queryKey: queryKeys.registries.all,
    queryFn: listRegistryCredentials,
  });
}
export function useSaveRegistryCredential() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({
      id,
      data,
    }: {
      id?: string;
      data: RegistryCredentialInput;
    }) =>
      id ? updateRegistryCredential(id, data) : createRegistryCredential(data),
    onSuccess: () =>
      client.invalidateQueries({ queryKey: queryKeys.registries.all }),
  });
}
export function useDeleteRegistryCredential() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: deleteRegistryCredential,
    onSuccess: () =>
      client.invalidateQueries({ queryKey: queryKeys.registries.all }),
  });
}
