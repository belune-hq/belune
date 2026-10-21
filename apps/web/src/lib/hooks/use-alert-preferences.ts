import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { getAlertPreferences, updateAlertPreferences } from "@/lib/api/alert-preferences";
import type { AlertPreferences } from "@/lib/types";
import { queryKeys } from "./query-keys";

export function useAlertPreferences() {
  return useQuery({
    queryKey: queryKeys.alertPreferences,
    queryFn: getAlertPreferences,
  });
}

export function useUpdateAlertPreferences() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (prefs: AlertPreferences) => updateAlertPreferences(prefs),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.alertPreferences });
    },
  });
}
