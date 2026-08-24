import { useState } from "react";
import { useNavigate } from "react-router-dom";
import * as auth from "@/api/auth";
import { useSession } from "@/lib/session";
import { Button } from "@/components/ui/Button";
import { Input, Label } from "@/components/ui/Input";
import { Panel } from "@/components/ui/Panel";
import { USE_MOCK } from "@/api/config";

export function LoginPage() {
  const { needsBootstrap, refresh } = useSession();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [member, setMember] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      if (needsBootstrap) {
        await auth.bootstrap({ name, member, password });
      } else {
        await auth.login({ member, password });
      }
      await refresh();
      navigate("/cases", { replace: true });
    } catch (err) {
      setError(err instanceof Error ? err.message : "Something went wrong");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex min-h-screen items-center justify-center bg-bg px-4">
      <Panel className="w-full max-w-sm p-6">
        <div className="mb-5 flex items-center gap-2">
          <div className="flex h-8 w-8 items-center justify-center rounded-[var(--radius-chip)] bg-brand-500 text-sm font-extrabold text-white">
            K
          </div>
          <span className="text-base font-extrabold tracking-tight">KARMAX Console</span>
        </div>

        {needsBootstrap ? (
          <p className="mb-4 text-sm text-fg-muted">
            No admin exists yet on this install — set up the first account.
          </p>
        ) : (
          <p className="mb-4 text-sm text-fg-muted">Sign in to run your agents.</p>
        )}

        <form className="space-y-3" onSubmit={submit}>
          {needsBootstrap && (
            <div className="space-y-1">
              <Label htmlFor="name">Your name</Label>
              <Input id="name" value={name} onChange={(e) => setName(e.target.value)} required />
            </div>
          )}
          <div className="space-y-1">
            <Label htmlFor="member">{needsBootstrap ? "Choose a username" : "Username"}</Label>
            <Input id="member" value={member} onChange={(e) => setMember(e.target.value)} required />
          </div>
          <div className="space-y-1">
            <Label htmlFor="password">Password</Label>
            <Input id="password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required />
          </div>
          {error && <p className="text-sm text-failed">{error}</p>}
          <Button type="submit" variant="primary" className="w-full" disabled={busy}>
            {busy ? "Working…" : needsBootstrap ? "Create admin account" : "Sign in"}
          </Button>
        </form>

        {USE_MOCK && (
          <p className="mt-4 text-xs text-fg-subtle">
            Demo mode — any username and password will do.
          </p>
        )}
      </Panel>
    </div>
  );
}
