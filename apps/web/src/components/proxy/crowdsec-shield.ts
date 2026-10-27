import { ShieldCheckIcon, ShieldIcon, ShieldOffIcon } from "lucide-react";
import type { CrowdSecShield } from "@/components/proxy/crowdsec-view";

/**
 * The glyph for each of CrowdSec's three states, in one place so the tab strip
 * and the card cannot drift apart. The rule that picks between them is
 * crowdSecShield; this is only the mapping to icons.
 */
export const CROWDSEC_SHIELD: Record<CrowdSecShield, typeof ShieldIcon> = {
  off: ShieldOffIcon,
  idle: ShieldIcon,
  enforcing: ShieldCheckIcon,
};
