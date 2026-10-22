import { useState } from "react";
import {
  ChevronDown,
  ChevronRight,
  Eye,
  EyeOff,
  Copy,
  Trash2,
} from "lucide-react";
import { Input } from "@/components/ui/input";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { IconAction } from "@/components/ui/icon-action";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupInput,
} from "../ui/input-group";
import { Field, FieldLabel } from "../ui/field";

export interface EnvVarCardModel {
  // Stable across a key's inherited -> override transition so React keeps the
  // same DOM node (and input focus) when a row is promoted in place.
  reactKey: string;
  key: string;
  keyEditable: boolean;
  onKeyChange?: (key: string) => void;
  value: string;
  onValueChange: (value: string) => void;
  isSecret: boolean;
  // Disabled while a saved secret is still masked — renaming the key or
  // flipping the secret flag without revealing first would re-encrypt the
  // literal "••••••••" mask as the row's new value.
  secretEditable: boolean;
  onSecretChange?: (isSecret: boolean) => void;
  revealed: boolean;
  revealable: boolean;
  onReveal?: () => Promise<void>;
  badge?: { label: string; variant: "secondary" | "outline" };
  onCopyValue: () => void;
  onCopyKeyValue: () => void;
  showTrash: boolean;
  trashLabel: string;
  onTrash?: () => void;
}

export function EnvVarCard({
  model,
  collapsed,
  onToggleCollapsed,
}: {
  model: EnvVarCardModel;
  collapsed: boolean;
  onToggleCollapsed: () => void;
}) {
  const [showPlain, setShowPlain] = useState(false);
  const [revealPending, setRevealPending] = useState(false);
  const locked = model.isSecret && !model.secretEditable;

  const handleEyeClick = async () => {
    if (model.isSecret && !model.revealed) {
      if (!model.onReveal) return;
      setRevealPending(true);
      try {
        await model.onReveal();
        setShowPlain(true);
      } finally {
        setRevealPending(false);
      }
    } else {
      setShowPlain((s) => !s);
    }
  };

  const inputType = model.isSecret && !showPlain ? "password" : "text";

  return (
    <div className="flex flex-col gap-2 rounded-lg border p-3">
      <div className="flex justify-between">
        <div className="flex items-center gap-2">
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={collapsed ? "Expand" : "Collapse"}
            onClick={onToggleCollapsed}
          >
            {collapsed ? (
              <ChevronRight className="size-3.5" />
            ) : (
              <ChevronDown className="size-3.5" />
            )}
          </Button>

          {collapsed && (
            <p className="truncate text-sm font-medium">
              {model.key || <span className="text-muted-foreground">KEY</span>}
              {!model.isSecret && model.value && (
                <>
                  <span className="ml-1">=</span>
                  <span className="text-muted-foreground ml-1 font-mono font-normal">
                    {model.value}
                  </span>
                </>
              )}
            </p>
          )}

          {model.isSecret && (
            <Badge variant="secondary" className="text-xs whitespace-nowrap">
              Secret
            </Badge>
          )}

          {model.badge && (
            <Badge
              variant={model.badge.variant}
              className="shrink-0 text-xs whitespace-nowrap"
            >
              {model.badge.label}
            </Badge>
          )}
        </div>

        {collapsed && (
          <div className="flex items-center gap-2">
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <Button variant="ghost" size="icon-sm" aria-label="Copy" />
                }
              >
                <Copy className="h-3.5 w-3.5" />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onClick={model.onCopyValue}>
                  Copy value
                </DropdownMenuItem>
                <DropdownMenuItem onClick={model.onCopyKeyValue}>
                  Copy as KEY=value
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
            <IconAction
              label={model.trashLabel}
              destructive
              size="icon-sm"
              onClick={() => model.onTrash?.()}
              disabled={!model.showTrash}
            >
              <Trash2 className="h-3.5 w-3.5" />
            </IconAction>
          </div>
        )}
      </div>

      {!collapsed && (
        <>
          <div className="flex items-center gap-2">
            <Field>
              <FieldLabel>Key</FieldLabel>
              <Input
                data-key-input={model.reactKey}
                placeholder="KEY"
                className="text-xs"
                value={model.key}
                disabled={!model.keyEditable}
                onChange={(e) => model.onKeyChange?.(e.target.value)}
              />
            </Field>

            <Field>
              <FieldLabel>Value</FieldLabel>
              <InputGroup>
                <InputGroupInput
                  placeholder={
                    model.isSecret && !model.revealed
                      ? "Type to Replace, or Reveal to Edit"
                      : "value"
                  }
                  className="font-mono text-xs"
                  type={inputType}
                  value={model.value}
                  onChange={(e) => model.onValueChange(e.target.value)}
                />

                {model.revealable && (
                  <InputGroupAddon align="inline-end">
                    <IconAction
                      label={
                        model.isSecret && !model.revealed
                          ? "Reveal"
                          : showPlain
                            ? "Hide"
                            : "Show"
                      }
                      size="icon-sm"
                      disabled={revealPending}
                      onClick={handleEyeClick}
                    >
                      {showPlain ? (
                        <EyeOff className="h-3.5 w-3.5" />
                      ) : (
                        <Eye className="h-3.5 w-3.5" />
                      )}
                    </IconAction>
                  </InputGroupAddon>
                )}
              </InputGroup>
            </Field>
          </div>

          <div className="flex items-center justify-between">
            <Label className="text-muted-foreground flex shrink-0 items-center gap-1 text-xs font-normal whitespace-nowrap">
              <Checkbox
                checked={model.isSecret}
                disabled={locked}
                onCheckedChange={(v) => model.onSecretChange?.(v === true)}
              />
              Secret
            </Label>

            <div>
              <DropdownMenu>
                <DropdownMenuTrigger
                  render={
                    <Button variant="ghost" size="icon-sm" aria-label="Copy" />
                  }
                >
                  <Copy className="h-3.5 w-3.5" />
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={model.onCopyValue}>
                    Copy value
                  </DropdownMenuItem>
                  <DropdownMenuItem onClick={model.onCopyKeyValue}>
                    Copy as KEY=value
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
              <IconAction
                label={model.trashLabel}
                destructive
                size="icon-sm"
                onClick={() => model.onTrash?.()}
                disabled={!model.showTrash}
              >
                <Trash2 className="h-3.5 w-3.5" />
              </IconAction>
            </div>
          </div>
        </>
      )}
    </div>
  );
}
