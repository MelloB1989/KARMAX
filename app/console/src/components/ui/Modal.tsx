import { type ReactNode, useEffect } from "react";
import { X } from "lucide-react";
import { cn } from "@/lib/utils";

export function Modal({
  open, onClose, title, children, wide = false,
}: { open: boolean; onClose: () => void; title: string; children: ReactNode; wide?: boolean }) {
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") onClose(); };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  if (!open) return null;
  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/30 p-4 pt-[8vh] animate-fade">
      <div
        className={cn(
          "panel w-full rounded-[var(--radius-panel)] p-5",
          wide ? "max-w-2xl" : "max-w-md",
        )}
        role="dialog"
        aria-modal="true"
        aria-label={title}
      >
        <div className="mb-4 flex items-center justify-between">
          <h2 className="text-h2">{title}</h2>
          <button
            onClick={onClose}
            className="rounded-full p-1 text-fg-muted hover:bg-surface-2 hover:text-fg"
            aria-label="Close"
          >
            <X className="h-4 w-4" />
          </button>
        </div>
        {children}
      </div>
    </div>
  );
}
