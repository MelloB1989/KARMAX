import type { LucideIcon } from "lucide-react";

export function EmptyState({
  icon: Icon, title, body,
}: { icon: LucideIcon; title: string; body?: string }) {
  return (
    <div className="flex flex-col items-center justify-center gap-2 py-16 text-center">
      <Icon className="h-8 w-8 text-fg-subtle" strokeWidth={1.5} />
      <p className="text-sm font-semibold text-fg-muted">{title}</p>
      {body && <p className="max-w-sm text-sm text-fg-subtle">{body}</p>}
    </div>
  );
}
