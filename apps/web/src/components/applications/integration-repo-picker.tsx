import { useEffect, useRef, useState } from "react";
import { Link } from "@tanstack/react-router";
import { FolderIcon, GitBranchIcon, Link2Icon } from "lucide-react";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  useGitIntegrations,
  useIntegrationRepos,
  useIntegrationBranches,
} from "@/lib/hooks/use-git-integrations";

interface Props {
  /** Called whenever a full connection + repo selection is made. */
  onSelect: (v: {
    integrationId: string;
    cloneUrl: string;
    branch: string;
  }) => void;
  /**
   * Pre-selects an existing app's account, repository, and branch when editing
   * (the create flow leaves these unset). repoFullName is the "owner/repo" the
   * app already points at; the branch is kept as-is rather than reset to the
   * repo default.
   */
  initialIntegrationId?: string;
  initialRepoFullName?: string;
  initialBranch?: string;
}

const PROVIDER_LABELS: Record<string, string> = {
  github: "GitHub",
  gitlab: "GitLab",
  bitbucket: "Bitbucket",
  gitea: "Gitea",
};

export function IntegrationRepoPicker({
  onSelect,
  initialIntegrationId = "",
  initialRepoFullName = "",
  initialBranch = "",
}: Props) {
  const { data: integrations } = useGitIntegrations();
  const [integrationId, setIntegrationId] = useState(initialIntegrationId);
  const [repoFullName, setRepoFullName] = useState(initialRepoFullName);
  const [branch, setBranch] = useState(initialBranch);

  // The "reset branch to the repo default" effect below must not fire for the
  // pre-selected repo — that would discard the app's actual branch on mount.
  // Skip exactly the first run when we started from an existing selection.
  const skipInitialBranchReset = useRef(Boolean(initialRepoFullName));

  const { data: repos } = useIntegrationRepos(integrationId || undefined);
  const { data: branches } = useIntegrationBranches(
    integrationId || undefined,
    repoFullName || undefined,
  );

  const selectedRepo = repos?.find((r) => r.full_name === repoFullName);

  // Emit upward whenever a complete selection exists.
  useEffect(() => {
    if (integrationId && selectedRepo && branch) {
      onSelect({
        integrationId,
        cloneUrl: selectedRepo.clone_url,
        branch,
      });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [integrationId, repoFullName, branch]);

  // Default the branch to the repo's default branch when the repo changes —
  // except on the initial pre-selected repo, whose branch we must preserve.
  useEffect(() => {
    if (skipInitialBranchReset.current) {
      skipInitialBranchReset.current = false;
      return;
    }
    if (selectedRepo) setBranch(selectedRepo.default_branch || "");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [repoFullName]);

  if (!integrations || integrations.length === 0) {
    return (
      <p className="text-muted-foreground text-sm">
        No connected accounts.{" "}
        <Link
          to="/git"
          search={{ tab: undefined, connected: undefined, error: undefined }}
          className="text-foreground font-medium underline underline-offset-2"
        >
          Connect an account
        </Link>{" "}
        under Git to pick a repository here.
      </p>
    );
  }

  return (
    <div className="space-y-3">
      <div className="space-y-2">
        <Label>Connection</Label>
        <Select
          value={integrationId}
          onValueChange={(v) => {
            setIntegrationId(v ?? "");
            setRepoFullName("");
            setBranch("");
          }}
        >
          <SelectTrigger>
            <SelectValue placeholder="Select a connected account" />
          </SelectTrigger>
          <SelectContent>
            {integrations.map((i) => (
              <SelectItem key={i.id} value={i.id} icon={<Link2Icon />}>
                {PROVIDER_LABELS[i.provider] ?? i.provider} · {i.account_login}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      {integrationId && (
        <div className="space-y-2">
          <Label>Repository</Label>
          <Select
            value={repoFullName}
            onValueChange={(v) => setRepoFullName(v ?? "")}
          >
            <SelectTrigger>
              <SelectValue placeholder="Select a repository" />
            </SelectTrigger>
            <SelectContent>
              {repos?.map((r) => (
                <SelectItem
                  key={r.full_name}
                  value={r.full_name}
                  icon={<FolderIcon />}
                >
                  {r.full_name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      )}

      {repoFullName && (
        <div className="space-y-2">
          <Label>Branch</Label>
          <Select value={branch} onValueChange={(v) => setBranch(v ?? "")}>
            <SelectTrigger>
              <SelectValue placeholder="Select a branch" />
            </SelectTrigger>
            <SelectContent>
              {branches?.map((b) => (
                <SelectItem
                  key={b.name}
                  value={b.name}
                  icon={<GitBranchIcon />}
                >
                  {b.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      )}
    </div>
  );
}
