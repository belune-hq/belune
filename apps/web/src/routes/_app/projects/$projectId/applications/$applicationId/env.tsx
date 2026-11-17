import { createFileRoute } from "@tanstack/react-router";
import { SlidersHorizontal } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { toast } from "sonner";
import {
  useEnvVars,
  useUpsertEnvVars,
  useRevealEnvVar,
} from "@/lib/hooks/use-envvars";
import {
  useProjectEnvVars,
  useRevealProjectEnvVar,
} from "@/lib/hooks/use-project-envvars";
import {
  useEnvVarDraft,
  type DraftEnvRow,
} from "@/components/env-vars/use-env-var-draft";
import {
  EnvVarCard,
  type EnvVarCardModel,
} from "@/components/env-vars/env-var-card";
import { EnvVarsActionBar } from "@/components/env-vars/env-vars-action-bar";
import {
  sortDraftRows,
  sortEnvVars,
  type EnvVarSortKey,
} from "@/components/env-vars/env-vars-sort";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { copyToClipboard } from "@/lib/utils/clipboard";
import { parseEnvContent } from "@/lib/utils/parse-env";
import type { EnvVar } from "@/lib/types";
import { RouteError } from "@/lib/components/route-error";

export const Route = createFileRoute(
  "/_app/projects/$projectId/applications/$applicationId/env",
)({
  component: EnvVarsPage,
  errorComponent: RouteError,
});

const ENV_KEY_REGEX = /^[A-Za-z_][A-Za-z0-9_]*$/;

function EnvVarsPage() {
  const { projectId, applicationId } = Route.useParams();
  const { data: envVars, isLoading } = useEnvVars(projectId, applicationId);
  const { data: projectEnvVars } = useProjectEnvVars(projectId);
  const upsert = useUpsertEnvVars(projectId, applicationId);
  const reveal = useRevealEnvVar(projectId, applicationId);
  const revealProject = useRevealProjectEnvVar(projectId);

  const draft = useEnvVarDraft(envVars);
  const [importOpen, setImportOpen] = useState(false);
  const [importText, setImportText] = useState("");
  const [search, setSearch] = useState("");
  // Cards start collapsed by default — tracking which are explicitly
  // expanded (rather than which are collapsed) makes that the natural
  // initial state instead of requiring every row to be seeded on load.
  const [expandedKeys, setExpandedKeys] = useState<Set<string>>(new Set());
  const [sortKey, setSortKey] = useState<EnvVarSortKey>("name");
  // Real values fetched for inherited (not-yet-overridden) secret rows, keyed
  // by the project env var id — kept out of the draft since these rows aren't
  // app-owned until the user actually edits one.
  const [revealedInherited, setRevealedInherited] = useState<
    Record<string, string>
  >({});

  const draftKeys = useMemo(
    () => new Set(draft.rows.map((r) => r.key)),
    [draft.rows],
  );
  const inheritedKeys = useMemo(
    () => new Set((projectEnvVars ?? []).map((v) => v.key)),
    [projectEnvVars],
  );
  const unshadowedInherited = useMemo(
    () => (projectEnvVars ?? []).filter((v) => !draftKeys.has(v.key)),
    [projectEnvVars, draftKeys],
  );

  const copyRow = async (row: DraftEnvRow, withKey: boolean) => {
    let value = row.value;
    if (row.is_secret && !row.revealed && row.id) {
      const res = await reveal.mutateAsync(row.id);
      value = res.value;
      draft.markRevealed(row.clientId, value);
    }
    const ok = await copyToClipboard(withKey ? `${row.key}=${value}` : value);
    if (ok) toast.success(withKey ? "Copied as KEY=value" : "Copied value");
    else toast.error("Copy failed");
  };

  const copyInherited = async (v: EnvVar, withKey: boolean) => {
    let value = revealedInherited[v.id];
    if (value === undefined) {
      if (v.is_secret) {
        const res = await revealProject.mutateAsync(v.id);
        value = res.value;
        setRevealedInherited((prev) => ({ ...prev, [v.id]: value! }));
      } else {
        value = v.value ?? "";
      }
    }
    const ok = await copyToClipboard(withKey ? `${v.key}=${value}` : value);
    if (ok) toast.success(withKey ? "Copied as KEY=value" : "Copied value");
    else toast.error("Copy failed");
  };

  const buildDraftModel = (row: DraftEnvRow): EnvVarCardModel => {
    const locked = row.is_secret && !row.revealed;
    const isOverride = inheritedKeys.has(row.key);
    return {
      // An override's key is fixed at creation (never typed live), so keying
      // by row.key here keeps the same DOM node as its inherited-card
      // predecessor. A plain row's key IS typed live, so it must key by the
      // stable clientId instead — keying by its in-progress value would
      // remount (and drop focus from) the input on every keystroke.
      reactKey: isOverride ? row.key : row.clientId,
      key: row.key,
      keyEditable: !locked,
      onKeyChange: (key) => draft.updateKey(row.clientId, key),
      value: row.value,
      onValueChange: (value) => draft.updateValue(row.clientId, value),
      isSecret: row.is_secret,
      secretEditable: !locked,
      onSecretChange: (v) => draft.updateSecret(row.clientId, v),
      revealed: row.revealed,
      revealable: row.is_secret && !!row.id,
      onReveal: row.id
        ? async () => {
            const res = await reveal.mutateAsync(row.id!);
            draft.markRevealed(row.clientId, res.value);
          }
        : undefined,
      badge: isOverride
        ? { label: "Overrides project", variant: "secondary" }
        : undefined,
      onCopyValue: () => copyRow(row, false),
      onCopyKeyValue: () => copyRow(row, true),
      showTrash: true,
      trashLabel: isOverride ? "Revert to inherited" : "Remove",
      onTrash: () => draft.removeRow(row.clientId),
    };
  };

  const buildInheritedModel = (v: EnvVar): EnvVarCardModel => {
    const revealedValue = revealedInherited[v.id];
    const revealed = !v.is_secret || revealedValue !== undefined;
    const displayValue = v.is_secret ? (revealedValue ?? "") : (v.value ?? "");
    const locked = v.is_secret && !revealed;

    return {
      reactKey: v.key,
      key: v.key,
      keyEditable: false,
      value: displayValue,
      onValueChange: (value) => draft.addOverride(v.key, v.is_secret, value),
      isSecret: v.is_secret,
      secretEditable: !locked,
      onSecretChange: (isSecret) =>
        draft.addOverride(v.key, isSecret, displayValue),
      revealed,
      revealable: v.is_secret,
      onReveal: async () => {
        const res = await revealProject.mutateAsync(v.id);
        setRevealedInherited((prev) => ({ ...prev, [v.id]: res.value }));
      },
      badge: { label: "Inherit from Project", variant: "outline" },
      onCopyValue: () => copyInherited(v, false),
      onCopyKeyValue: () => copyInherited(v, true),
      showTrash: false,
      trashLabel:
        "Inherited — edit the value to override, then remove the override",
    };
  };

  // Inherited rows render above app-owned rows, with a divider between —
  // inherited vars are the project's baseline, app vars are what layers on
  // top of it. Sorting applies within each group rather than across the
  // whole list, so that split stays meaningful regardless of sort order.
  const inheritedModels = sortEnvVars(unshadowedInherited, sortKey).map(
    buildInheritedModel,
  );
  const draftModels = sortDraftRows(draft.rows, sortKey).map(buildDraftModel);
  const allModels = [...inheritedModels, ...draftModels];

  const q = search.trim().toLowerCase();
  const matchesQuery = (m: EnvVarCardModel) =>
    !q || m.key.toLowerCase().includes(q);
  const filteredInherited = inheritedModels.filter(matchesQuery);
  const filteredDraft = draftModels.filter(matchesQuery);
  const filtered = [...filteredInherited, ...filteredDraft];

  const allCollapsed =
    filtered.length > 0 && filtered.every((m) => !expandedKeys.has(m.reactKey));

  const toggleCollapseAll = () => {
    setExpandedKeys(
      allCollapsed ? new Set(filtered.map((m) => m.reactKey)) : new Set(),
    );
  };
  const toggleCollapse = (key: string) => {
    setExpandedKeys((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  // Set right after adding a row; the effect below waits for that row to
  // actually be in the DOM (it renders expanded on the same pass) before
  // scrolling to it and focusing its Key field. Never reset to null — each
  // clientId comes from a page-lifetime counter, so it never repeats and the
  // effect's dependency diff alone is enough to fire exactly once per add.
  const [pendingFocusKey, setPendingFocusKey] = useState<string | null>(null);

  useEffect(() => {
    if (!pendingFocusKey) return;
    const input = document.querySelector<HTMLInputElement>(
      `[data-key-input="${pendingFocusKey}"]`,
    );
    input?.scrollIntoView({ behavior: "smooth", block: "center" });
    // Plain focus() scrolls the element into view itself — instantly,
    // hijacking the smooth scroll started above mid-animation.
    input?.focus({ preventScroll: true });
  }, [pendingFocusKey]);

  const handleAdd = () => {
    const clientId = draft.addRow();
    setExpandedKeys((prev) => new Set(prev).add(clientId));
    setPendingFocusKey(clientId);
  };

  const handleImport = () => {
    const parsed = parseEnvContent(importText);
    if (parsed.length === 0) return;
    draft.importParsed(parsed);
    setImportText("");
    setImportOpen(false);
  };

  const handleSave = () => {
    const payload = draft.buildPayload();
    const invalid = payload.find((r) => !ENV_KEY_REGEX.test(r.key));
    if (invalid) {
      toast.error(`Invalid variable name: ${invalid.key}`);
      return;
    }
    toast.promise(upsert.mutateAsync(payload), {
      loading: "Saving variables...",
      success: "Environment variables saved",
      error: (err) => err.message,
    });
  };

  if (isLoading) {
    return (
      <Card>
        <CardHeader>
          <Skeleton className="h-6 w-48" />
        </CardHeader>
        <CardContent className="space-y-2">
          {[1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-14 w-full" />
          ))}
        </CardContent>
      </Card>
    );
  }

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center justify-between">
          <CardTitle className="flex items-center gap-2">
            <SlidersHorizontal aria-hidden="true" className="size-4" />
            Environment Variables
          </CardTitle>
          <Button
            size="sm"
            variant="outline"
            onClick={() => setImportOpen(true)}
          >
            Paste .env
          </Button>
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        <EnvVarsActionBar
          variant="top"
          search={search}
          onSearchChange={setSearch}
          allCollapsed={allCollapsed}
          onToggleCollapseAll={toggleCollapseAll}
          sortKey={sortKey}
          onSortKeyChange={setSortKey}
          onAdd={handleAdd}
          onSave={handleSave}
          saving={upsert.isPending}
        />

        {filtered.length === 0 ? (
          <p className="text-muted-foreground py-8 text-center text-sm">
            {allModels.length === 0
              ? 'No environment variables. Click "Add" to add one.'
              : "No variables match your search."}
          </p>
        ) : (
          <div className="space-y-2">
            {filteredInherited.map((m) => (
              <EnvVarCard
                key={m.reactKey}
                model={m}
                collapsed={!expandedKeys.has(m.reactKey)}
                onToggleCollapsed={() => toggleCollapse(m.reactKey)}
              />
            ))}
            {filteredInherited.length > 0 && filteredDraft.length > 0 && (
              <Separator className="my-4" />
            )}
            {filteredDraft.map((m) => (
              <EnvVarCard
                key={m.reactKey}
                model={m}
                collapsed={!expandedKeys.has(m.reactKey)}
                onToggleCollapsed={() => toggleCollapse(m.reactKey)}
              />
            ))}
          </div>
        )}

        {filtered.length > 10 && (
          <EnvVarsActionBar
            variant="bottom"
            onAdd={handleAdd}
            onSave={handleSave}
            saving={upsert.isPending}
          />
        )}
      </CardContent>

      <Dialog open={importOpen} onOpenChange={setImportOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Import .env</DialogTitle>
            <DialogDescription>
              Paste your .env file content below. Existing variables with
              matching keys will be updated.
            </DialogDescription>
          </DialogHeader>
          <Textarea
            className="min-h-50 font-mono text-xs"
            placeholder={
              "# Paste your .env content\nKEY=value\nANOTHER_KEY=another_value"
            }
            value={importText}
            onChange={(e) => setImportText(e.target.value)}
          />
          <DialogFooter>
            <Button variant="outline" onClick={() => setImportOpen(false)}>
              Cancel
            </Button>
            <Button onClick={handleImport} disabled={!importText.trim()}>
              Import
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
}
