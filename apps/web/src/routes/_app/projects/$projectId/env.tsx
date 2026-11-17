import { createFileRoute } from "@tanstack/react-router";
import { SlidersHorizontal } from "lucide-react";
import { useEffect, useState } from "react";
import { toast } from "sonner";
import {
  useProjectEnvVars,
  useUpsertProjectEnvVars,
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
  type EnvVarSortKey,
} from "@/components/env-vars/env-vars-sort";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
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
import { RouteError } from "@/lib/components/route-error";

export const Route = createFileRoute("/_app/projects/$projectId/env")({
  component: ProjectEnvVarsPage,
  errorComponent: RouteError,
});

const ENV_KEY_REGEX = /^[A-Za-z_][A-Za-z0-9_]*$/;

function ProjectEnvVarsPage() {
  const { projectId } = Route.useParams();
  const { data: envVars, isLoading } = useProjectEnvVars(projectId);
  const upsert = useUpsertProjectEnvVars(projectId);
  const reveal = useRevealProjectEnvVar(projectId);

  const draft = useEnvVarDraft(envVars);
  const [importOpen, setImportOpen] = useState(false);
  const [importText, setImportText] = useState("");
  const [search, setSearch] = useState("");
  // Cards start collapsed by default — tracking which are explicitly
  // expanded (rather than which are collapsed) makes that the natural
  // initial state instead of requiring every row to be seeded on load.
  const [expandedKeys, setExpandedKeys] = useState<Set<string>>(new Set());
  const [sortKey, setSortKey] = useState<EnvVarSortKey>("name");

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

  const buildModel = (row: DraftEnvRow): EnvVarCardModel => {
    const locked = row.is_secret && !row.revealed;
    return {
      // clientId, not key — the key field is edited live, and keying the
      // list item on its current value would remount (and drop focus from)
      // the input on every keystroke.
      reactKey: row.clientId,
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
      onCopyValue: () => copyRow(row, false),
      onCopyKeyValue: () => copyRow(row, true),
      showTrash: true,
      trashLabel: "Remove",
      onTrash: () => draft.removeRow(row.clientId),
    };
  };

  const allModels = sortDraftRows(draft.rows, sortKey).map(buildModel);
  const q = search.trim().toLowerCase();
  const filtered = q
    ? allModels.filter((m) => m.key.toLowerCase().includes(q))
    : allModels;

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
          <Skeleton className="h-6 w-56" />
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
          <div>
            <CardTitle className="flex items-center gap-2">
              <SlidersHorizontal aria-hidden="true" className="size-4" />
              Project Environment Variables
            </CardTitle>
            <p className="text-muted-foreground mt-1 text-sm">
              These variables are inherited by all applications in this project.
              Application-level variables with the same key will override these.
            </p>
          </div>
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
              ? 'No project environment variables. Click "Add" to add one.'
              : "No variables match your search."}
          </p>
        ) : (
          <div className="space-y-2">
            {filtered.map((m) => (
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
