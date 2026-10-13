import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { queryKeys } from "./query-keys";
import * as applicationsApi from "@/lib/api/applications";

export function useApplications(projectId: string) {
  return useQuery({
    queryKey: queryKeys.applications.all(projectId),
    queryFn: () => applicationsApi.listApplications(projectId),
    // A steady interval, not a status-driven one. The previous condition
    // looked for "building" | "deploying" | "pending" — those are DEPLOYMENT
    // statuses; an application's status is only inactive | running | stopped |
    // error, so the fast branch never once fired. Live changes arrive over the
    // WebSocket; this is the safety net for a missed event.
    refetchInterval: 10000,
  });
}

export function useApplication(projectId: string, applicationId: string) {
  return useQuery({
    queryKey: queryKeys.applications.detail(projectId, applicationId),
    queryFn: () => applicationsApi.getApplication(projectId, applicationId),
    // Same fix as above, and this one was worse: the dead condition made this
    // return `false` every time, so the detail page never refreshed itself at
    // all. It looked fine only because the WebSocket pushes status changes.
    refetchInterval: 15000,
  });
}

export function useCreateApplication(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (
      data: Parameters<typeof applicationsApi.createApplication>[1],
    ) => applicationsApi.createApplication(projectId, data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.applications.all(projectId) });
      qc.invalidateQueries({ queryKey: queryKeys.projects.detail(projectId) });
    },
    onError: (error) => toast.error(error.message),
  });
}

export function useUpdateApplication(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (
      data: Parameters<typeof applicationsApi.updateApplication>[2],
    ) => applicationsApi.updateApplication(projectId, applicationId, data),
    // ⚠️ One call, not two. invalidateQueries matches by PREFIX, and `all` is
    // ["projects", p, "applications"] — a prefix of `detail` and of every key
    // nested under it (deployments, domains, cache). So this already covers
    // them; adding `detail` re-marked those four while the first batch was
    // still in flight and fetched each of them a second time. Measured on a
    // real save: 1 PUT + 9 GETs, four of them pure waste.
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: queryKeys.applications.all(projectId) }),
    onError: (error) => toast.error(error.message),
  });
}

export function useUpdateApplicationRuntime(
  projectId: string,
  applicationId: string,
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (
      data: Parameters<typeof applicationsApi.updateApplicationRuntime>[2],
    ) =>
      applicationsApi.updateApplicationRuntime(projectId, applicationId, data),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.applications.detail(projectId, applicationId),
      });
    },
    onError: (error) => toast.error(error.message),
  });
}

export function useDeleteApplication(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (applicationId: string) =>
      applicationsApi.deleteApplication(projectId, applicationId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.applications.all(projectId) });
      qc.invalidateQueries({ queryKey: queryKeys.projects.detail(projectId) });
    },
    onError: (error) => toast.error(error.message),
  });
}

export function useDeployApplication(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () =>
      applicationsApi.deployApplication(projectId, applicationId),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.applications.detail(projectId, applicationId),
      });
      qc.invalidateQueries({
        queryKey: queryKeys.deployments.all(projectId, applicationId),
      });
    },
    onError: (error) => toast.error(error.message),
  });
}

export function useStopApplication(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => applicationsApi.stopApplication(projectId, applicationId),
    // Await BOTH the detail and list refetch so the mutation stays `isPending`
    // until the row's status actually reflects the change. Without awaiting the
    // list query, isPending flips false while the list still shows the old
    // status, briefly rendering the wrong action buttons (e.g. Stop/Restart
    // flashing between the spinner and Start when stopping).
    onSuccess: () =>
      Promise.all([
        qc.invalidateQueries({
          queryKey: queryKeys.applications.detail(projectId, applicationId),
        }),
        qc.invalidateQueries({
          queryKey: queryKeys.applications.all(projectId),
        }),
      ]),
    onError: (error) => toast.error(error.message),
  });
}

export function useStartApplication(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () =>
      applicationsApi.startApplication(projectId, applicationId),
    // Await BOTH the detail and list refetch so the mutation stays `isPending`
    // until the row's status actually reflects the change. Without awaiting the
    // list query, isPending flips false while the list still shows the old
    // status, briefly rendering the wrong action buttons (e.g. Stop/Restart
    // flashing between the spinner and Start when stopping).
    onSuccess: () =>
      Promise.all([
        qc.invalidateQueries({
          queryKey: queryKeys.applications.detail(projectId, applicationId),
        }),
        qc.invalidateQueries({
          queryKey: queryKeys.applications.all(projectId),
        }),
      ]),
    onError: (error) => toast.error(error.message),
  });
}

export function useRestartApplication(
  projectId: string,
  applicationId: string,
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () =>
      applicationsApi.restartApplication(projectId, applicationId),
    // Await BOTH the detail and list refetch so the mutation stays `isPending`
    // until the row's status actually reflects the change. Without awaiting the
    // list query, isPending flips false while the list still shows the old
    // status, briefly rendering the wrong action buttons (e.g. Stop/Restart
    // flashing between the spinner and Start when stopping).
    onSuccess: () =>
      Promise.all([
        qc.invalidateQueries({
          queryKey: queryKeys.applications.detail(projectId, applicationId),
        }),
        qc.invalidateQueries({
          queryKey: queryKeys.applications.all(projectId),
        }),
      ]),
    onError: (error) => toast.error(error.message),
  });
}

// Reload and Rebuild both create a new deployment, so they invalidate the
// deployments list as well as the application detail (mirrors useDeployApplication).
export function useReloadApplication(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () =>
      applicationsApi.reloadApplication(projectId, applicationId),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.applications.detail(projectId, applicationId),
      });
      qc.invalidateQueries({
        queryKey: queryKeys.deployments.all(projectId, applicationId),
      });
    },
    onError: (error) => toast.error(error.message),
  });
}

export function useRebuildApplication(
  projectId: string,
  applicationId: string,
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () =>
      applicationsApi.rebuildApplication(projectId, applicationId),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.applications.detail(projectId, applicationId),
      });
      qc.invalidateQueries({
        queryKey: queryKeys.deployments.all(projectId, applicationId),
      });
    },
    onError: (error) => toast.error(error.message),
  });
}

export function useBuildCache(projectId: string, applicationId: string) {
  return useQuery({
    queryKey: queryKeys.applications.buildCache(projectId, applicationId),
    queryFn: () => applicationsApi.getBuildCache(projectId, applicationId),
  });
}

export function useClearBuildCache(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => applicationsApi.clearBuildCache(projectId, applicationId),
    onSuccess: () =>
      qc.invalidateQueries({
        queryKey: queryKeys.applications.buildCache(projectId, applicationId),
      }),
    onError: (error) => toast.error(error.message),
  });
}

// A mutation, not a query: each call writes an audit-log entry server-side, so
// the secret is fetched only on an explicit click.
export function useRevealWebhookSecret(
  projectId: string,
  applicationId: string,
) {
  return useMutation({
    mutationFn: () =>
      applicationsApi.revealWebhookSecret(projectId, applicationId),
    onError: (error) => toast.error(error.message),
  });
}

// Reports whether the deploy hook is enabled. Never carries the token itself —
// use useRevealDeployHook for that.
export function useDeployHook(projectId: string, applicationId: string) {
  return useQuery({
    queryKey: queryKeys.applications.deployHook(projectId, applicationId),
    queryFn: () => applicationsApi.getDeployHook(projectId, applicationId),
  });
}

export function useGenerateDeployHook(
  projectId: string,
  applicationId: string,
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () =>
      applicationsApi.generateDeployHook(projectId, applicationId),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.applications.deployHook(projectId, applicationId),
      });
    },
    onError: (error) => toast.error(error.message),
  });
}

// A mutation rather than a query so the token is fetched only on an explicit
// click: each call writes an audit-log entry server-side.
export function useRevealDeployHook(projectId: string, applicationId: string) {
  return useMutation({
    mutationFn: () =>
      applicationsApi.revealDeployHook(projectId, applicationId),
    onError: (error) => toast.error(error.message),
  });
}

export function useDeleteDeployHook(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () =>
      applicationsApi.deleteDeployHook(projectId, applicationId),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.applications.deployHook(projectId, applicationId),
      });
    },
    onError: (error) => toast.error(error.message),
  });
}

export function useUpdateWebhook(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: Parameters<typeof applicationsApi.updateWebhook>[2]) =>
      applicationsApi.updateWebhook(projectId, applicationId, data),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.applications.detail(projectId, applicationId),
      });
    },
    onError: (error) => toast.error(error.message),
  });
}

export function useChangeApplicationSource(
  projectId: string,
  applicationId: string,
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (
      data: Parameters<typeof applicationsApi.changeApplicationSource>[2],
    ) =>
      applicationsApi.changeApplicationSource(projectId, applicationId, data),
    // The list carries `type` too, and the settings page renders entirely
    // different fields either side of the switch.
    onSuccess: () =>
      Promise.all([
        qc.invalidateQueries({
          queryKey: queryKeys.applications.detail(projectId, applicationId),
        }),
        qc.invalidateQueries({
          queryKey: queryKeys.applications.all(projectId),
        }),
      ]),
    onError: (error) => toast.error(error.message),
  });
}

export function useSetHealthCheck(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: Parameters<typeof applicationsApi.setHealthCheck>[2]) =>
      applicationsApi.setHealthCheck(projectId, applicationId, data),
    onSuccess: () =>
      qc.invalidateQueries({
        queryKey: queryKeys.applications.detail(projectId, applicationId),
      }),
    onError: (error) => toast.error(error.message),
  });
}

export function useSetResources(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: Parameters<typeof applicationsApi.setResources>[2]) =>
      applicationsApi.setResources(projectId, applicationId, data),
    onSuccess: () =>
      qc.invalidateQueries({
        queryKey: queryKeys.applications.detail(projectId, applicationId),
      }),
    onError: (error) => toast.error(error.message),
  });
}
