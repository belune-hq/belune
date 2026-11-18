import { api } from "./client";

export interface RegistryCredential {
  id: string;
  registry_host: string;
  username: string;
  password_set: boolean;
  created_at: string;
  updated_at: string;
}

export interface RegistryCredentialInput {
  registry_host: string;
  username: string;
  password?: string;
}

export const listRegistryCredentials = () =>
  api.get<RegistryCredential[]>("/settings/registries");
export const createRegistryCredential = (data: RegistryCredentialInput) =>
  api.post<RegistryCredential>("/settings/registries", data);
export const updateRegistryCredential = (
  id: string,
  data: RegistryCredentialInput,
) => api.put<RegistryCredential>(`/settings/registries/${id}`, data);
export const deleteRegistryCredential = (id: string) =>
  api.delete<void>(`/settings/registries/${id}`);
