import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { queryKeys } from "./query-keys";
import * as channelsApi from "@/lib/api/notification-channels";
import type { SaveNotificationChannel } from "@/lib/api/notification-channels";

export function useNotificationEvents() {
  return useQuery({
    queryKey: queryKeys.notificationEvents,
    queryFn: () => channelsApi.listNotificationEvents(),
    // The registry is static for a given build — no need to refetch often.
    staleTime: 60 * 60 * 1000,
  });
}

export function useNotificationChannels() {
  return useQuery({
    queryKey: queryKeys.notificationChannels,
    queryFn: () => channelsApi.listNotificationChannels(),
  });
}

export function useCreateNotificationChannel() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: SaveNotificationChannel) =>
      channelsApi.createNotificationChannel(data),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: queryKeys.notificationChannels }),
  });
}

export function useUpdateNotificationChannel() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, data }: { id: string; data: SaveNotificationChannel }) =>
      channelsApi.updateNotificationChannel(id, data),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: queryKeys.notificationChannels }),
  });
}

export function useSetNotificationChannelEnabled() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      channelsApi.setNotificationChannelEnabled(id, enabled),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: queryKeys.notificationChannels }),
  });
}

export function useDeleteNotificationChannel() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => channelsApi.deleteNotificationChannel(id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: queryKeys.notificationChannels }),
  });
}

export function useTestNotificationChannel() {
  return useMutation({
    mutationFn: (id: string) => channelsApi.testNotificationChannel(id),
  });
}

export function useTestNotificationChannelParams() {
  return useMutation({
    mutationFn: (data: Parameters<typeof channelsApi.testNotificationChannelParams>[0]) =>
      channelsApi.testNotificationChannelParams(data),
  });
}
