/**
 * The retention knobs on the Server page: what each one is called, what it
 * defaults to, and what the API will accept for it.
 *
 * Split out of the route so the presets can be checked against the bounds in a
 * test. They were not, and "Forever" sat in the list for several releases
 * offering a value the API rejects with a 400.
 */

/** A choice in a retention dropdown. */
export interface RetentionPreset {
  label: string;
  value: string;
}

/** One retention setting, as the page presents it. */
export interface RetentionField {
  key: string;
  label: string;
  desc: string;
  fallback: string;
}

/**
 * Day-based presets for the log rows.
 *
 * ⚠️ One list, three fields, and they do NOT accept the same range: WAF
 * detections top out at 365 days where the others take 3650, so **nothing above
 * 365 may be added here** without splitting the list. A value this list offers
 * and one of its fields rejects is a dropdown entry that 400s, which is what
 * "Forever" was.
 *
 * ⛔ And "Forever" cannot come back as 0. Every reader of these settings treats
 * a non-positive value as unset and falls back to its default (see the
 * day-based retention case in handler/settings.go), so 0 would not mean "keep
 * everything" even if the validator let it through — it would mean "sweep at
 * the default" while the page said otherwise. Keeping data forever would need a
 * sentinel the sweeps understand, not a zero.
 */
export const RETENTION_PRESETS: RetentionPreset[] = [
  { label: "3 days", value: "3" },
  { label: "7 days", value: "7" },
  { label: "30 days", value: "30" },
  { label: "90 days", value: "90" },
  { label: "1 year", value: "365" },
];

/** Host metrics are 1-second data, so they use a short hours-based window. */
export const HOST_METRICS_PRESETS: RetentionPreset[] = [
  { label: "1 day", value: "24" },
  { label: "2 days", value: "48" },
];

export const RETENTION_FIELDS: RetentionField[] = [
  {
    key: "app_log_retention_days",
    label: "Application logs",
    desc: "stdout / stderr per container",
    fallback: "7",
  },
  {
    key: "request_log_retention_days",
    label: "Request logs",
    desc: "HTTP access logs per application",
    // Matches the WAF detection window: a detection's request detail is joined
    // from this table, so a shorter window here blanks the evidence first.
    fallback: "7",
  },
  {
    key: "waf_events_retention_days",
    label: "WAF detections",
    desc: "attacks the WAF would have blocked (1–365)",
    fallback: "7",
  },
];

export const HOST_METRICS_FIELD: RetentionField = {
  key: "host_metrics_retention_hours",
  label: "Host metrics (1-second)",
  desc: "CPU / memory / disk time-series",
  fallback: "24",
};

/**
 * What the API accepts for each key, mirrored from the retention cases in
 * handler/settings.go. Duplicated across the language boundary on purpose: the
 * point is to fail a test here when the page offers something the server
 * refuses, which no amount of Go-side validation can do.
 */
export const RETENTION_BOUNDS: Record<string, { min: number; max: number }> = {
  app_log_retention_days: { min: 1, max: 3650 },
  request_log_retention_days: { min: 1, max: 3650 },
  waf_events_retention_days: { min: 1, max: 365 },
  host_metrics_retention_hours: { min: 1, max: 87600 },
};
