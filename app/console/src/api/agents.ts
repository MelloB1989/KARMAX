import { USE_MOCK } from "./config";
import { get } from "./client";
import type { AgentDetail, AgentSummary } from "./types";
import { AGENTS, AGENT_SUMMARIES } from "./mock/data";
import { delay } from "./mock/util";

export async function listAgents(): Promise<AgentSummary[]> {
  if (USE_MOCK) return delay(AGENT_SUMMARIES);
  return get<{ agents: AgentSummary[] }>("/api/console/agents").then((r) => r.agents);
}

export async function getAgent(id: string): Promise<AgentDetail | null> {
  if (USE_MOCK) return delay(AGENTS.find((a) => a.id === id) ?? null);
  return get<AgentDetail>(`/api/console/agents/${encodeURIComponent(id)}`);
}
