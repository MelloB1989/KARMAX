import { LogOut } from "lucide-react";
import { ThemeToggle } from "./ThemeToggle";
import { USE_MOCK } from "@/api/config";
import { useSession } from "@/lib/session";

export function TopBar() {
  const { session, signOut } = useSession();
  return (
    <header className="flex h-13 shrink-0 items-center justify-between border-b border-border bg-bg-soft px-5">
      <div className="flex items-center gap-2">
        <span className="text-[13px] font-semibold text-fg">Acme Inc</span>
        {USE_MOCK && (
          <span className="rounded-[3px] border border-border px-2 py-0.5 font-mono text-[10.5px] text-fg-subtle">
            demo data · no backend
          </span>
        )}
      </div>
      <div className="flex items-center gap-3">
        <ThemeToggle />
        {session && (
          <div className="flex items-center gap-2 border-l border-border pl-3">
            <span className="text-[12.5px] text-fg-muted">{session.name}</span>
            <button
              onClick={signOut}
              aria-label="Sign out"
              className="rounded-md p-1.5 text-fg-subtle hover:bg-surface-2 hover:text-fg"
            >
              <LogOut className="h-4 w-4" />
            </button>
          </div>
        )}
      </div>
    </header>
  );
}
