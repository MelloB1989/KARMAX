import { useEffect, useState } from "react";
import { Moon, Sun, SunMoon } from "lucide-react";
import { applyTheme, getThemePref, setThemePref, type ThemePref } from "@/lib/theme";
import { cn } from "@/lib/utils";

const OPTIONS: { pref: ThemePref; icon: typeof Sun; label: string }[] = [
  { pref: "light", icon: Sun, label: "Light" },
  { pref: "system", icon: SunMoon, label: "System" },
  { pref: "dark", icon: Moon, label: "Dark" },
];

export function ThemeToggle() {
  const [pref, setPref] = useState<ThemePref>(getThemePref());

  useEffect(() => { applyTheme(pref); }, [pref]);

  return (
    <div className="flex items-center gap-0.5 rounded-[var(--radius-chip)] border border-border bg-surface-2 p-0.5">
      {OPTIONS.map(({ pref: p, icon: Icon, label }) => (
        <button
          key={p}
          type="button"
          aria-label={label}
          aria-pressed={pref === p}
          onClick={() => { setPref(p); setThemePref(p); }}
          className={cn(
            "flex h-6 w-6 items-center justify-center rounded-full transition-colors",
            pref === p ? "bg-bg text-fg shadow-sm" : "text-fg-subtle hover:text-fg",
          )}
        >
          <Icon className="h-3.5 w-3.5" />
        </button>
      ))}
    </div>
  );
}
