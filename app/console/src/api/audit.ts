import { USE_MOCK } from "./config";
import { get } from "./client";
import type { AuditEvent } from "./types";
import { AUDIT_EVENTS } from "./mock/data";
import { delay } from "./mock/util";

export interface AuditFilter {
  actor_id?: string;
  agent?: string;
  case_id?: string;
  verb?: string;
  since?: string;
  limit?: number;
}

export async function queryAudit(f: AuditFilter = {}): Promise<AuditEvent[]> {
  if (USE_MOCK) {
    let out = AUDIT_EVENTS.slice();
    if (f.actor_id) out = out.filter((e) => e.actor_id === f.actor_id);
    if (f.agent) out = out.filter((e) => e.agent === f.agent);
    if (f.case_id) out = out.filter((e) => e.case_id === f.case_id);
    if (f.verb) out = out.filter((e) => e.verb.includes(f.verb!));
    if (f.since) out = out.filter((e) => e.created_at >= f.since!);
    out.sort((a, b) => b.created_at.localeCompare(a.created_at));
    if (f.limit) out = out.slice(0, f.limit);
    return delay(out);
  }
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(f)) if (v !== undefined && v !== "") qs.set(k, String(v));
  const q = qs.toString();
  return get<{ events: AuditEvent[] }>(`/api/console/audit${q ? `?${q}` : ""}`).then((r) => r.events);
}
