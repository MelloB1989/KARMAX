import { cn } from "@/lib/utils";

export type ChipOption = { value: string; label: string };

// A filter you can read without opening it.
//
// A native <select> hides the available values behind a click and shows only
// the current one, which is the wrong trade on a board an operator scans all
// day: the set of agents IS small and IS worth seeing. Chips also let the eye
// land on "which agents exist" and "what state am I filtered to" in the same
// glance, and they stay in the console's mono-as-structure vocabulary instead
// of importing the browser's.
export function ChipGroup({
  label,
  value,
  options,
  onChange,
  className,
}: {
  label: string;
  value: string;
  options: ChipOption[];
  onChange: (value: string) => void;
  className?: string;
}) {
  return (
    <div className={cn("flex items-center gap-1.5", className)}>
      <span className="mr-0.5 text-label font-semibold uppercase text-fg-subtle">{label}</span>
      {options.map((o) => {
        const active = o.value === value;
        return (
          <button
            key={o.value || "__all"}
            type="button"
            onClick={() => onChange(o.value)}
            aria-pressed={active}
            className={cn(
              "rounded-[4px] border px-2 py-[3px] font-mono text-[11px] transition-colors",
              active
                ? "border-border-strong bg-surface-2 text-fg"
                : "border-transparent text-fg-muted hover:bg-surface hover:text-fg",
            )}
          >
            {o.label}
          </button>
        );
      })}
    </div>
  );
}
