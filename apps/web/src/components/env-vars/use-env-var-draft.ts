import { useCallback, useState } from "react";
import { newClientId } from "@/lib/utils/client-id";
import type { EnvVar, EnvVarInput } from "@/lib/types";

export interface DraftEnvRow {
  clientId: string;
  // Present once this row exists server-side; absent for a row added in this
  // session that hasn't been saved yet.
  id?: string;
  key: string;
  value: string;
  is_secret: boolean;
  // True once the user has changed key/value/secret since load. A row that's
  // never gone dirty is sent to the backend as "unchanged" so a masked secret
  // never gets re-encrypted from its own mask — see prepareEnvVars server-side.
  dirty: boolean;
  // True once `value` holds real plaintext rather than the list endpoint's
  // "••••••••" mask — always true for non-secret rows and brand-new rows.
  revealed: boolean;
  // Absent for a row added in this session that hasn't been saved yet — see
  // env-vars-sort.ts for how that's treated when sorting by time.
  createdAt?: string;
  updatedAt?: string;
}

function fromServer(v: EnvVar): DraftEnvRow {
  return {
    clientId: v.id,
    id: v.id,
    key: v.key,
    value: v.value ?? "",
    is_secret: v.is_secret,
    dirty: false,
    revealed: !v.is_secret,
    createdAt: v.created_at,
    updatedAt: v.updated_at,
  };
}

// useEnvVarDraft owns the editable draft for one env-var list (an
// application's own vars, or a project's vars) independent of search/collapse
// UI state, so filtering the view never risks dropping a row from Save.
export function useEnvVarDraft(serverVars: EnvVar[] | undefined) {
  const [rows, setRows] = useState<DraftEnvRow[]>([]);

  // Syncs the draft to a new server snapshot (initial load, or after a
  // refetch actually changes the list). Applied during render rather than in
  // an effect — React's documented pattern for resetting state from a prop —
  // so the draft never briefly shows a stale row after commit.
  const [syncedServerVars, setSyncedServerVars] = useState(serverVars);
  if (serverVars && serverVars !== syncedServerVars) {
    setSyncedServerVars(serverVars);
    setRows(serverVars.map(fromServer));
  }

  const addRow = useCallback(() => {
    const clientId = newClientId();
    setRows((prev) => [
      ...prev,
      {
        clientId,
        key: "",
        value: "",
        is_secret: false,
        dirty: false,
        revealed: true,
      },
    ]);
    return clientId;
  }, []);

  // Adds a row for a key that previously only existed as an inherited
  // project var — the app-level override the "edit inherited row in place"
  // flow creates.
  const addOverride = useCallback(
    (key: string, is_secret: boolean, value: string) => {
      setRows((prev) => [
        ...prev,
        {
          clientId: newClientId(),
          key,
          value,
          is_secret,
          dirty: true,
          revealed: true,
        },
      ]);
    },
    [],
  );

  const removeRow = useCallback((clientId: string) => {
    setRows((prev) => prev.filter((r) => r.clientId !== clientId));
  }, []);

  const updateKey = useCallback((clientId: string, key: string) => {
    setRows((prev) =>
      prev.map((r) =>
        r.clientId === clientId ? { ...r, key, dirty: true } : r,
      ),
    );
  }, []);

  const updateValue = useCallback((clientId: string, value: string) => {
    setRows((prev) =>
      prev.map((r) =>
        r.clientId === clientId
          ? { ...r, value, dirty: true, revealed: true }
          : r,
      ),
    );
  }, []);

  const updateSecret = useCallback((clientId: string, is_secret: boolean) => {
    setRows((prev) =>
      prev.map((r) =>
        r.clientId === clientId ? { ...r, is_secret, dirty: true } : r,
      ),
    );
  }, []);

  // Drops a freshly-fetched real value into a row without marking it dirty —
  // the user asked to see it, not to change it, so an un-re-edited reveal
  // still saves as "unchanged".
  const markRevealed = useCallback((clientId: string, value: string) => {
    setRows((prev) =>
      prev.map((r) =>
        r.clientId === clientId ? { ...r, value, revealed: true } : r,
      ),
    );
  }, []);

  const importParsed = useCallback(
    (parsed: { key: string; value: string }[]) => {
      setRows((prev) => {
        const updated = [...prev];
        for (const { key, value } of parsed) {
          const existing = updated.findIndex((r) => r.key === key);
          if (existing !== -1) {
            updated[existing] = {
              ...updated[existing],
              value,
              dirty: true,
              revealed: true,
            };
          } else {
            updated.push({
              clientId: newClientId(),
              key,
              value,
              is_secret: false,
              dirty: true,
              revealed: true,
            });
          }
        }
        return updated;
      });
    },
    [],
  );

  const buildPayload = useCallback((): EnvVarInput[] => {
    return rows
      .filter((r) => r.key.trim())
      .map((r) => {
        const unchanged = !r.dirty && !!r.id;
        return {
          key: r.key,
          value: unchanged ? "" : r.value,
          is_secret: r.is_secret,
          ...(unchanged ? { unchanged: true as const } : {}),
        };
      });
  }, [rows]);

  return {
    rows,
    addRow,
    addOverride,
    removeRow,
    updateKey,
    updateValue,
    updateSecret,
    markRevealed,
    importParsed,
    buildPayload,
  };
}
