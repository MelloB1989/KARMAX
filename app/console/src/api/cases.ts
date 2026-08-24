import { USE_MOCK } from "./config";
import { get } from "./client";
import type { Case, CaseDetail } from "./types";
import { CASES, CASE_EVENTS, SANDBOX_RUNS } from "./mock/data";
import { delay } from "./mock/util";

export interface ListCasesParams {
  agent?: string;
  state?: string;
  limit?: number;
}

export async function listCases(params: ListCasesParams = {}): Promise<Case[]> {
  if (USE_MOCK) {
    let out = CASES.slice();
    if (params.agent) out = out.filter((c) => c.agent === params.agent);
    if (params.state) out = out.filter((c) => c.state === params.state);
    out.sort((a, b) => b.updated_at.localeCompare(a.updated_at));
    if (params.limit) out = out.slice(0, params.limit);
    return delay(out);
  }
  const qs = new URLSearchParams();
  if (params.agent) qs.set("agent", params.agent);
  if (params.state) qs.set("state", params.state);
  if (params.limit) qs.set("limit", String(params.limit));
  const q = qs.toString();
  return get<{ cases: Case[] }>(`/api/console/cases${q ? `?${q}` : ""}`).then((r) => r.cases);
}

export async function getCase(id: string): Promise<CaseDetail | null> {
  if (USE_MOCK) {
    const found = CASES.find((c) => c.id === id || c.key === id);
    if (!found) return delay(null);
    return delay({
      case: found,
      events: (CASE_EVENTS[found.id] ?? []).slice().sort((a, b) => a.created_at.localeCompare(b.created_at)),
      sandbox_runs: SANDBOX_RUNS[found.id] ?? [],
    });
  }
  return get<CaseDetail>(`/api/console/cases/${encodeURIComponent(id)}`);
}
