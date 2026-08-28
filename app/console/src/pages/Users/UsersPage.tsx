import { useEffect, useState } from "react";
import { KeyRound, Trash2, UserPlus, Users as UsersIcon } from "lucide-react";
import { createUser, deleteUser, listUsers, setPassword, updateUser } from "@/api/users";
import type { ConsoleUser, ConsoleUsers } from "@/api/types";
import { Panel } from "@/components/ui/Panel";
import { Button } from "@/components/ui/Button";
import { Input, Label } from "@/components/ui/Input";
import { Badge } from "@/components/ui/Badge";
import { PageSpinner } from "@/components/ui/Spinner";
import { EmptyState } from "@/components/ui/EmptyState";

const ROLE_TONE: Record<string, "brand" | "neutral"> = { admin: "brand", operator: "neutral", viewer: "neutral" };

// What each role can actually do, said once here rather than left for someone
// to infer from a dropdown.
const ROLE_HELP: Record<string, string> = {
  viewer: "Read everything. Change nothing.",
  operator: "Approve actions, run workflows, edit connectors.",
  admin: "Everything, plus users and the organisation.",
};

export function UsersPage() {
  const [data, setData] = useState<ConsoleUsers | null>(null);
  const [err, setErr] = useState("");
  const [adding, setAdding] = useState(false);
  const [draft, setDraft] = useState({ member: "", name: "", role: "viewer", password: "", email: "" });
  const [pwFor, setPwFor] = useState<ConsoleUser | null>(null);
  const [pw, setPw] = useState({ current_password: "", password: "" });
  const [pwMsg, setPwMsg] = useState("");

  const refresh = () => listUsers().then(setData).catch((e) => setErr(String(e)));
  useEffect(() => { void refresh(); }, []);

  if (!data) return <PageSpinner />;

  const run = async (fn: () => Promise<unknown>) => {
    setErr("");
    try {
      await fn();
      await refresh();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <div>
      <div className="mb-5 flex items-start justify-between gap-4">
        <div>
          <h1 className="text-h1">Users</h1>
          <p className="text-sm text-fg-muted">
            Who can sign in to this console, and what they may do once they have.
          </p>
        </div>
        <Button onClick={() => setAdding((v) => !v)}>
          <UserPlus className="mr-1.5 h-4 w-4" />
          Add user
        </Button>
      </div>

      {err && (
        <Panel className="mb-4 border-failed/40 p-3">
          <p className="text-sm text-failed">{err}</p>
        </Panel>
      )}

      {adding && (
        <Panel className="mb-4 p-5">
          <h2 className="mb-3 text-h2">New user</h2>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <div className="space-y-1">
              <Label htmlFor="member">Member ID *</Label>
              <Input id="member" value={draft.member} placeholder="priya"
                onChange={(e) => setDraft({ ...draft, member: e.target.value })} />
            </div>
            <div className="space-y-1">
              <Label htmlFor="uname">Name</Label>
              <Input id="uname" value={draft.name} placeholder="Priya S"
                onChange={(e) => setDraft({ ...draft, name: e.target.value })} />
            </div>
            <div className="space-y-1">
              <Label htmlFor="urole">Role</Label>
              <select
                id="urole"
                value={draft.role}
                onChange={(e) => setDraft({ ...draft, role: e.target.value })}
                className="h-9 w-full rounded-[var(--radius-field)] border border-border bg-surface px-2.5 text-sm text-fg outline-none focus:border-brand"
              >
                {data.roles.map((r) => <option key={r} value={r}>{r}</option>)}
              </select>
            </div>
            <div className="space-y-1">
              <Label htmlFor="upw">Password</Label>
              <Input id="upw" type="password" value={draft.password} placeholder="at least 8 characters"
                onChange={(e) => setDraft({ ...draft, password: e.target.value })} />
            </div>
            <div className="space-y-1">
              <Label htmlFor="uemail">Google sign-in address</Label>
              <Input id="uemail" value={draft.email} placeholder="priya@acme.com"
                onChange={(e) => setDraft({ ...draft, email: e.target.value })} />
            </div>
          </div>
          <p className="mt-2 text-xs text-fg-subtle">{ROLE_HELP[draft.role]}</p>
          <p className="mt-1 text-xs text-fg-subtle">
            Give them a password, a Google address, or both — an account with neither has no way
            in. An address alone means they sign in with Google and have no password to leak.
          </p>
          <div className="mt-3 flex gap-2">
            <Button
              disabled={
                !draft.member ||
                (draft.password.length > 0 && draft.password.length < 8) ||
                (draft.password.length === 0 && draft.email.trim() === "")
              }
              onClick={() => run(async () => {
                await createUser(draft);
                setDraft({ member: "", name: "", role: "viewer", password: "", email: "" });
                setAdding(false);
              })}
            >
              Create
            </Button>
            <Button variant="ghost" onClick={() => setAdding(false)}>Cancel</Button>
          </div>
        </Panel>
      )}

      {data.users.length === 0 ? (
        <EmptyState icon={UsersIcon} title="No users" />
      ) : (
        <Panel className="overflow-hidden">
          <table className="w-full text-sm">
            <thead className="border-b border-border text-left text-xs uppercase tracking-wide text-fg-subtle">
              <tr>
                <th className="px-4 py-2.5 font-medium">Member</th>
                <th className="px-4 py-2.5 font-medium">Name</th>
                <th className="px-4 py-2.5 font-medium">Google sign-in</th>
                <th className="px-4 py-2.5 font-medium">Role</th>
                <th className="px-4 py-2.5" />
              </tr>
            </thead>
            <tbody>
              {data.users.map((u) => (
                <tr key={u.member} className="border-b border-border last:border-0">
                  <td className="px-4 py-2.5 font-mono text-xs text-fg">
                    {u.member}
                    {u.self && <span className="ml-2 text-fg-subtle">(you)</span>}
                  </td>
                  <td className="px-4 py-2.5 text-fg-muted">{u.name || "—"}</td>
                  <td className="px-4 py-2.5">
                    <Input
                      defaultValue={u.email}
                      placeholder="—"
                      className="h-7 w-52 text-xs"
                      onBlur={(e) => {
                        if (e.target.value !== u.email) void run(() => updateUser(u.member, { email: e.target.value }));
                      }}
                    />
                  </td>
                  <td className="px-4 py-2.5">
                    <select
                      value={u.role}
                      onChange={(e) => run(() => updateUser(u.member, { role: e.target.value }))}
                      className="h-7 rounded-[var(--radius-field)] border border-border bg-surface px-2 text-xs text-fg outline-none focus:border-brand"
                    >
                      {data.roles.map((r) => <option key={r} value={r}>{r}</option>)}
                    </select>
                    <Badge tone={ROLE_TONE[u.role] ?? "neutral"} className="ml-2 align-middle">{u.role}</Badge>
                  </td>
                  <td className="px-4 py-2.5">
                    <div className="flex justify-end gap-1.5">
                      <Button variant="ghost" onClick={() => { setPwFor(u); setPw({ current_password: "", password: "" }); setPwMsg(""); }}>
                        <KeyRound className="h-3.5 w-3.5" />
                      </Button>
                      {!u.self && (
                        <Button variant="ghost" onClick={() => run(() => deleteUser(u.member))}>
                          <Trash2 className="h-3.5 w-3.5 text-failed" />
                        </Button>
                      )}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Panel>
      )}

      {pwFor && (
        <Panel className="mt-4 p-5">
          <h2 className="mb-1 text-h2">
            {pwFor.self ? "Change your password" : `Reset ${pwFor.member}'s password`}
          </h2>
          <p className="mb-3 text-sm text-fg-muted">
            Every session this account holds is signed out
            {pwFor.self ? ", including this one — you will need to sign in again." : "."}
          </p>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            {pwFor.self && (
              <div className="space-y-1">
                <Label htmlFor="cur">Current password</Label>
                <Input id="cur" type="password" value={pw.current_password}
                  onChange={(e) => setPw({ ...pw, current_password: e.target.value })} />
              </div>
            )}
            <div className="space-y-1">
              <Label htmlFor="new">New password</Label>
              <Input id="new" type="password" value={pw.password} placeholder="at least 8 characters"
                onChange={(e) => setPw({ ...pw, password: e.target.value })} />
            </div>
          </div>
          {pwMsg && <p className="mt-2 text-sm text-healthy">{pwMsg}</p>}
          <div className="mt-3 flex gap-2">
            <Button
              disabled={pw.password.length < 8 || (pwFor.self && !pw.current_password)}
              onClick={() => run(async () => {
                const res = await setPassword(pwFor.member, pw);
                setPwMsg(res.sign_in_again ? "Changed. Sign in again." : "Changed.");
                if (!res.sign_in_again) setPwFor(null);
              })}
            >
              Change password
            </Button>
            <Button variant="ghost" onClick={() => setPwFor(null)}>Cancel</Button>
          </div>
        </Panel>
      )}
    </div>
  );
}
