import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import type { Session } from "@/api/types";
import * as auth from "@/api/auth";

interface SessionCtx {
  session: Session | null;
  loading: boolean;
  needsBootstrap: boolean;
  refresh: () => Promise<void>;
  signOut: () => void;
}

const Ctx = createContext<SessionCtx | null>(null);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<Session | null>(null);
  const [loading, setLoading] = useState(true);
  const [needsBootstrap, setNeedsBootstrap] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const s = await auth.me();
      setSession(s);
      if (!s) {
        const status = await auth.bootstrapStatus();
        setNeedsBootstrap(status.needs_bootstrap);
      }
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void refresh(); }, [refresh]);

  const signOut = useCallback(() => {
    auth.logout();
    setSession(null);
    void refresh();
  }, [refresh]);

  return (
    <Ctx.Provider value={{ session, loading, needsBootstrap, refresh, signOut }}>
      {children}
    </Ctx.Provider>
  );
}

export function useSession(): SessionCtx {
  const ctx = useContext(Ctx);
  if (!ctx) throw new Error("useSession must be used inside SessionProvider");
  return ctx;
}
