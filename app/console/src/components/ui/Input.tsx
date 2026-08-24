import { forwardRef, type InputHTMLAttributes, type TextareaHTMLAttributes, type SelectHTMLAttributes, type LabelHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

const fieldClass =
  "w-full rounded-[var(--radius-card)] border border-border bg-bg px-2.5 py-1.5 text-[13px] text-fg placeholder:text-fg-subtle outline-none transition-colors focus:border-border-glow disabled:opacity-50";

export const Input = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement>>(
  ({ className, ...props }, ref) => (
    <input ref={ref} className={cn(fieldClass, className)} {...props} />
  ),
);
Input.displayName = "Input";

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaHTMLAttributes<HTMLTextAreaElement>>(
  ({ className, ...props }, ref) => (
    <textarea ref={ref} className={cn(fieldClass, "resize-y", className)} {...props} />
  ),
);
Textarea.displayName = "Textarea";

// The browser's own select chrome is the one thing on these screens that does
// not belong to the console — a different radius, a different arrow, a
// different type ramp on every OS. appearance-none drops it; the chevron below
// is ours.
export const Select = forwardRef<HTMLSelectElement, SelectHTMLAttributes<HTMLSelectElement>>(
  ({ className, children, ...props }, ref) => (
    // The caller's className sizes the WRAPPER, not the control: the chevron is
    // positioned against the wrapper, so a width left on the inner select would
    // park the arrow at the far edge of a box the select does not fill.
    <div className={cn("relative inline-flex", className)}>
      <select
        ref={ref}
        className={cn(fieldClass, "appearance-none pr-7 font-mono text-[11.5px] text-fg-muted")}
        {...props}
      >
        {children}
      </select>
      <svg
        aria-hidden="true"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        className="pointer-events-none absolute right-2 top-1/2 h-3 w-3 -translate-y-1/2 text-fg-subtle"
      >
        <path d="M6 9l6 6 6-6" />
      </svg>
    </div>
  ),
);
Select.displayName = "Select";

export function Label(props: LabelHTMLAttributes<HTMLLabelElement>) {
  return <label className="text-xs font-semibold text-fg-muted" {...props} />;
}
