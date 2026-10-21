import { useState } from "react";
import { Copy, Check } from "lucide-react";
import { Button } from "@/components/ui/button";
import { copyToClipboard } from "@/lib/utils/clipboard";
import {
  Tooltip,
  TooltipContent,
  TooltipPositioner,
  TooltipTrigger,
} from "@/components/ui/tooltip";

interface CopyButtonProps {
  value: string;
  /** Size variant — defaults to "icon" (square icon button) */
  size?: "icon" | "sm";
  className?: string;
}

/**
 * A small button that copies `value` to the clipboard and briefly shows a
 * checkmark confirmation. A tooltip reads "Copy" and switches to "Copied!"
 * for ~2s after a successful copy. Uses the navigator.clipboard API with a
 * silent fallback on failure.
 */
export function CopyButton({ value, size = "icon", className }: CopyButtonProps) {
  const [copied, setCopied] = useState(false);
  const [hoverOpen, setHoverOpen] = useState(false);

  const handleCopy = async () => {
    const ok = await copyToClipboard(value);
    if (ok) {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }
  };

  // Force the tooltip open while "Copied!" is showing — base-ui closes the
  // tooltip on click, so we override its open state for the confirmation window.
  return (
    <Tooltip open={hoverOpen || copied} onOpenChange={setHoverOpen}>
      <TooltipTrigger
        render={
          <Button
            variant="ghost"
            size={size === "icon" ? "icon" : "sm"}
            className={size === "icon" ? `h-7 w-7 ${className ?? ""}` : className}
            onClick={handleCopy}
            aria-label={copied ? "Copied!" : "Copy to clipboard"}
          />
        }
      >
        {copied ? (
          <Check className="h-3.5 w-3.5 text-green-500" />
        ) : (
          <Copy className="h-3.5 w-3.5" />
        )}
      </TooltipTrigger>
      <TooltipPositioner>
        <TooltipContent>{copied ? "Copied!" : "Copy"}</TooltipContent>
      </TooltipPositioner>
    </Tooltip>
  );
}
