import { USE_MOCK, getToken } from "./config";
import { get, post } from "./client";
import type { Approval, ApprovalStatus } from "./types";
import { APPROVALS } from "./mock/data";
import { delay } from "./mock/util";

export async function listApprovals(status?: ApprovalStatus): Promise<Approval[]> {
  if (USE_MOCK) {
    let out = APPROVALS.slice();
    if (status) out = out.filter((a) => a.status === status);
    out.sort((a, b) => b.created_at.localeCompare(a.created_at));
    return delay(out);
  }
  const q = status ? `?status=${status}` : "";
  return get<{ approvals: Approval[] }>(`/api/console/approvals${q}`).then((r) => r.approvals);
}

export interface DecideApprovalInput {
  decision: "approve" | "reject";
  note?: string;
}

export async function decideApproval(id: string, input: DecideApprovalInput): Promise<Approval> {
  if (USE_MOCK) {
    const a = APPROVALS.find((x) => x.id === id);
    if (!a) throw new Error("no such approval: " + id);
    a.status = input.decision === "approve" ? "approved" : "rejected";
    a.note = input.note ?? "";
    a.decided_at = new Date().toISOString();
    a.decided_by = getToken() ? "you" : "operator";
    return delay(a, 400);
  }
  return post<Approval>(`/api/console/approvals/${encodeURIComponent(id)}/decision`, input);
}
