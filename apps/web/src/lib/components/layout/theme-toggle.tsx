import { useTheme } from "next-themes";
import { Sun, Moon, Monitor, Check } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";

const THEMES = [
  { value: "light", label: "Light", Icon: Sun },
  { value: "dark", label: "Dark", Icon: Moon },
  { value: "system", label: "System", Icon: Monitor },
] as const;

/** Compact icon-button theme switcher for the top bar. */
export function ThemeToggle() {
  const { theme, setTheme } = useTheme();

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={<Button variant="ghost" size="icon" aria-label="Toggle theme" />}
      >
        {/* Icon crossfades on the `.dark` class set by next-themes — no JS state. */}
        <span className="relative flex h-4 w-4 items-center justify-center">
          <Sun
            aria-hidden="true"
            className="h-4 w-4 rotate-0 scale-100 transition-all dark:-rotate-90 dark:scale-0"
          />
          <Moon
            aria-hidden="true"
            className="absolute h-4 w-4 rotate-90 scale-0 transition-all dark:rotate-0 dark:scale-100"
          />
        </span>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-36">
        {THEMES.map(({ value, label, Icon }) => (
          <DropdownMenuItem key={value} onClick={() => setTheme(value)}>
            <Icon aria-hidden="true" className="h-4 w-4" />
            {label}
            {theme === value && (
              <Check aria-hidden="true" className="ml-auto h-4 w-4" />
            )}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
