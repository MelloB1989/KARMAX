import { cn } from "@/lib/utils";
import type { CaseState } from "@/api/types";

// State reads three ways at once: a glyph, a word, and (on a row) a stripe.
// That is deliberate — a filled pill per row turns a dense board into a
// christmas tree, and colour alone excludes anyone who cannot separate amber
// from green. Only the two states that want attention right now carry colour:
// building, where an agent is mid-flight, and review, where a human is the
// thing being waited on.
const CONFIG: Record<CaseState, { label: string; glyph: string; fg: string; stripe: string; spin?: boolean }> = {
  open:     { label: "open",     glyph: "○", fg: "text-fg-subtle", stripe: "bg-border" },
  grooming: { label: "grooming", glyph: "◌", fg: "text-fg-muted",  stripe: "bg-border-strong" },
  ready:    { label: "ready",    glyph: "△", fg: "text-healthy",   stripe: "bg-healthy" },
  building: { label: "building", glyph: "◐", fg: "text-brand-500", stripe: "bg-brand-500", spin: true },
  review:   { label: "review",   glyph: "◎", fg: "text-sun-500",   stripe: "bg-sun-500" },
  done:     { label: "done",     glyph: "●", fg: "text-fg-subtle", stripe: "bg-border" },
  dropped:  { label: "dropped",  glyph: "⊘", fg: "text-fg-subtle", stripe: "bg-border" },
};

export function caseStateStripe(state: CaseState): string {
  return (CONFIG[state] ?? CONFIG.open).stripe;
}

export function CaseStateChip({ state, className }: { state: CaseState; className?: string }) {
  const cfg = CONFIG[state] ?? CONFIG.open;
  return (
    <span className={cn("inline-flex items-center gap-2 font-mono text-[11px]", cfg.fg, className)}>
      <span className={cn("text-[11px] leading-none", cfg.spin && "motion-safe:animate-pulse-soft")}>
        {cfg.glyph}
      </span>
      {cfg.label}
    </span>
  );
}

export const CASE_STATES: CaseState[] = ["open", "grooming", "ready", "building", "review", "done", "dropped"];
