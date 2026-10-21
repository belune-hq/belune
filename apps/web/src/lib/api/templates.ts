import { api } from "./client";

export interface TemplateSummary {
  id: string;
  name: string;
  description: string;
  category: string;
  logo_url?: string;
  website?: string;
  version?: string;
  docs_url?: string;
  tags?: string[];
  databases: number;
  services: number;
}

export interface TemplateInput {
  key: string;
  label: string;
  description?: string;
  required?: boolean;
  validation?: "" | "email" | "url";
  default?: string;
}

export interface TemplateDatabaseSpec {
  name: string;
  engine: string;
  version?: string;
}

export interface TemplateDetail extends TemplateSummary {
  needs_hostname: boolean;
  inputs?: TemplateInput[];
  notes?: string;
  service_names: string[];
  database_specs?: TemplateDatabaseSpec[];
}

export interface InstantiateTemplateRequest {
  project_id?: string;
  new_project_name?: string;
  hostname?: string;
  inputs: Record<string, string>;
}

export interface InstantiateTemplateResponse {
  project_id: string;
  project_slug: string;
  notes?: string;
}

export function listTemplates() {
  return api.get<TemplateSummary[]>("/templates");
}

export function getTemplate(id: string) {
  return api.get<TemplateDetail>(`/templates/${id}`);
}

export function instantiateTemplate(id: string, data: InstantiateTemplateRequest) {
  return api.post<InstantiateTemplateResponse>(`/templates/${id}/instantiate`, data);
}
