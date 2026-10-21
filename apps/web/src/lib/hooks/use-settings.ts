import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { queryKeys } from "./query-keys";
import * as settingsApi from "@/lib/api/settings";
import type { SettingEntry } from "@/lib/types";

export function useSettings() {
  return useQuery({
    queryKey: queryKeys.settings,
    queryFn: settingsApi.getSettings,
  });
}

export function useUpdateSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (settings: SettingEntry[]) => settingsApi.updateSettings(settings),
    onSuccess: () => qc.invalidateQueries({ queryKey: queryKeys.settings }),
  });
}
