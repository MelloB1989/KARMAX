import type { HTMLAttributes } from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

const badge = cva(
  "inline-flex items-center gap-1 rounded-[var(--radius-chip)] px-2 py-0.5 text-xs font-medium",
  {
    variants: {
      tone: {
        neutral: "bg-surface-2 text-fg-muted border border-border",
        brand: "bg-brand-100 text-brand-700",
        healthy: "bg-healthy-bg text-healthy",
        degraded: "bg-degraded-bg text-degraded",
        failed: "bg-failed-bg text-failed",
        unknown: "bg-unknown-bg text-unknown",
      },
    },
    defaultVariants: { tone: "neutral" },
  },
);

export interface BadgeProps extends HTMLAttributes<HTMLSpanElement>, VariantProps<typeof badge> {}

export function Badge({ className, tone, ...props }: BadgeProps) {
  return <span className={cn(badge({ tone }), className)} {...props} />;
}
