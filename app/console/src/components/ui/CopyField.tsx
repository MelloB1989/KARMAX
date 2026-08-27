import { useState } from "react";
import { Check, Copy } from "lucide-react";

export function CopyField({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard permission denied — nothing to fall back to here */
    }
  };
  return (
    <div className="flex items-center gap-2 rounded-[var(--radius-card)] border border-border bg-bg-soft px-3 py-2">
      <code className="min-w-0 flex-1 truncate font-mono text-[13px] text-fg">{value}</code>
      <button
        onClick={copy}
        className="shrink-0 rounded-md p-1 text-fg-muted hover:bg-surface-2 hover:text-fg"
        aria-label="Copy"
        type="button"
      >
        {copied ? <Check className="h-3.5 w-3.5 text-healthy" /> : <Copy className="h-3.5 w-3.5" />}
      </button>
    </div>
  );
}
