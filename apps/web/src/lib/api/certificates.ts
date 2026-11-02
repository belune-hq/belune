import { api } from "./client";

export interface Certificate {
  id: string;
  name: string;
  issuer: string;
  subjects: string[];
  not_before: string | null;
  not_after: string | null;
  domain_count: number;
  created_at: string;
}

export interface UploadCertificate {
  name: string;
  cert_pem: string;
  key_pem: string;
}

export function listCertificates() {
  return api.get<Certificate[]>("/certificates");
}

export function uploadCertificate(data: UploadCertificate) {
  return api.post<Certificate>("/certificates", data);
}

export function deleteCertificate(id: string) {
  return api.delete<void>(`/certificates/${id}`);
}

/** One row of the central Domain TLS view. */
export interface DomainTLSStatus {
  id: string;
  hostname: string;
  ssl_mode: string;
  tls_status: string;
  tls_issuer?: string;
  tls_not_after: string | null;
  tls_last_checked_at: string | null;
  tls_error?: string;
  /** A suspicion that explains a pending domain; never a verdict. */
  tls_advisory?: string;
  certificate_name?: string;
  application_id: string;
  application_name: string;
  /** Not running means no route, and so no certificate is requested. */
  application_status: string;
  project_id: string;
}

/** The reduced shape a non-admin may see. Deliberately no `subjects`: a
 *  certificate names every host it was issued for, and the API withholds that
 *  from anyone who cannot already list certificates. */
export interface UsableCertificate {
  id: string;
  name: string;
  issuer: string;
  not_after: string | null;
}

export function listUsableCertificates(hostname: string) {
  return api.get<UsableCertificate[]>(
    `/certificates/usable?hostname=${encodeURIComponent(hostname)}`,
  );
}

export function listDomainTLSStatus() {
  return api.get<DomainTLSStatus[]>("/domains/tls");
}

export function recheckDomainTLS(
  projectId: string,
  applicationId: string,
  domainId: string,
) {
  return api.post<void>(
    `/projects/${projectId}/applications/${applicationId}/domains/${domainId}/tls/recheck`,
    {},
  );
}
