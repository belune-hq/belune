import { Palette, Check } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { useAccentStore, type Accent } from "@/lib/stores/accent";

const ACCENTS: { value: Accent; label: string; swatch: string }[] = [
  { value: "violet", label: "Violet", swatch: "#7c3aed" },
  { value: "emerald", label: "Emerald", swatch: "#10b981" },
];

export function AccentToggle() {
  const accent = useAccentStore((s) => s.accent);
  const setAccent = useAccentStore((s) => s.setAccent);

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button variant="ghost" size="icon" aria-label="Change accent color" />
        }
      >
        <Palette aria-hidden="true" className="h-4 w-4" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-36">
        {ACCENTS.map(({ value, label, swatch }) => (
          <DropdownMenuItem key={value} onClick={() => setAccent(value)}>
            <span
              aria-hidden="true"
              className="size-3.5 rounded-full"
              style={{ background: swatch }}
            />
            {label}
            {accent === value && (
              <Check aria-hidden="true" className="ml-auto h-4 w-4" />
            )}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
