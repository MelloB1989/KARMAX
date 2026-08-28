import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import * as auth from "@/api/auth";
import { useSession } from "@/lib/session";
import { Button } from "@/components/ui/Button";
import { Input, Label } from "@/components/ui/Input";
import { Panel } from "@/components/ui/Panel";
import { PageSpinner } from "@/components/ui/Spinner";
import { USE_MOCK } from "@/api/config";
import type { GoogleSignIn } from "@/api/types";

export function LoginPage() {
  const { needsBootstrap, refresh } = useSession();
  const navigate = useNavigate();
  const [google, setGoogle] = useState<GoogleSignIn>({ enabled: false, domain: "" });
  const [returning, setReturning] = useState(true);
  const [name, setName] = useState("");
  const [member, setMember] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    // A sign-in that just came back from Google leaves its session in the URL
    // fragment. Consume it before anything else so the page does not flash a
    // login form at somebody who has already signed in.
    (async () => {
      if (auth.consumeGoogleSignIn()) {
        await refresh();
        navigate("/cases", { replace: true });
        return;
      }
      setReturning(false);
      setGoogle(await auth.googleSignInStatus());
    })();
  }, [refresh, navigate]);

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

  // Somebody coming back from Google already has a session in the fragment.
  // Showing them a login form for a moment before redirecting looks like the
  // sign-in failed.
  if (returning) {
    return <div className="flex min-h-screen items-center justify-center bg-bg"><PageSpinner /></div>;
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-bg px-4">
      <Panel className="w-full max-w-sm p-6">
        <div className="mb-5 flex items-center gap-2">
          <div className="flex h-8 w-8 items-center justify-center rounded-[var(--radius-chip)] bg-brand-500 text-sm font-extrabold text-white">
            o
          </div>
          <span className="text-base font-extrabold tracking-tight">oCrew Console</span>
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

        {/* Bootstrap creates the FIRST admin, and that has to be a password:
            there is no account yet for a Google identity to match against, and
            provisioning an admin from an email domain would hand the install to
            whoever found the URL first. */}
        {google.enabled && !needsBootstrap && (
          <>
            <div className="my-4 flex items-center gap-3">
              <span className="h-px flex-1 bg-border" />
              <span className="text-xs text-fg-subtle">or</span>
              <span className="h-px flex-1 bg-border" />
            </div>
            <Button
              type="button"
              variant="ghost"
              className="w-full"
              disabled={busy}
              onClick={async () => {
                setError("");
                setBusy(true);
                try {
                  window.location.href = await auth.startGoogleSignIn();
                } catch (err) {
                  setError(err instanceof Error ? err.message : "Could not start sign-in");
                  setBusy(false);
                }
              }}
            >
              <svg className="mr-2 h-4 w-4" viewBox="0 0 24 24" aria-hidden="true">
                <path fill="#4285F4" d="M23.5 12.3c0-.8-.1-1.6-.2-2.3H12v4.5h6.5a5.6 5.6 0 0 1-2.4 3.6v3h3.9c2.3-2.1 3.5-5.2 3.5-8.8z"/>
                <path fill="#34A853" d="M12 24c3.2 0 5.9-1.1 7.9-2.9l-3.9-3a7.2 7.2 0 0 1-10.7-3.8h-4v3.1A12 12 0 0 0 12 24z"/>
                <path fill="#FBBC05" d="M5.3 14.3a7.1 7.1 0 0 1 0-4.6v-3.1h-4a12 12 0 0 0 0 10.8l4-3.1z"/>
                <path fill="#EA4335" d="M12 4.8c1.8 0 3.4.6 4.6 1.8l3.4-3.4A12 12 0 0 0 1.3 6.6l4 3.1A7.2 7.2 0 0 1 12 4.8z"/>
              </svg>
              Continue with Google
              {google.domain && <span className="ml-1 text-fg-subtle">({google.domain})</span>}
            </Button>
          </>
        )}

        {USE_MOCK && (
          <p className="mt-4 text-xs text-fg-subtle">
            Demo mode — any username and password will do.
          </p>
        )}
      </Panel>
    </div>
  );
}
