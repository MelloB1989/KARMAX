// The console's action routes, which proxy to fleetd and return its reply.
async function post<T>(path: string, body: unknown): Promise<T> {
  const r = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  const j = (await r.json().catch(() => ({}))) as T & { error?: string };
  if (!r.ok) throw new Error(j.error ?? `${path}: ${r.status}`);
  return j;
}

export const prompt = (agent: string, text: string) => post<{ ok: boolean }>("/v1/prompt", { agent, text });

export interface RotateReply {
  ok: boolean;
  closes?: string;
  when?: string;
}
/** With dryRun, fleetd only says what a rotation would do ({closes, when}); nothing changes. */
export const rotate = (agent: string, dryRun = false) =>
  post<RotateReply>("/v1/rotate", dryRun ? { agent, dry_run: true } : { agent });
