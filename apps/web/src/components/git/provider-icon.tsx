import {
  SiGithub,
  SiGitlab,
  SiBitbucket,
  SiGitea,
} from "@icons-pack/react-simple-icons";
import { GitBranch } from "lucide-react";
import type { ComponentType } from "react";

const ICONS: Record<
  string,
  ComponentType<{ size?: number; className?: string; color?: string }>
> = {
  github: SiGithub,
  gitlab: SiGitlab,
  bitbucket: SiBitbucket,
  gitea: SiGitea,
};

/**
 * Brand icon for a git provider. Rendered monochrome (currentColor) so it reads
 * correctly in both light and dark themes; falls back to a generic git glyph for
 * unknown providers.
 */
export function ProviderIcon({
  provider,
  className,
}: {
  provider: string;
  className?: string;
}) {
  const Icon = ICONS[provider] ?? GitBranch;
  return <Icon color="currentColor" className={className} />;
}
