import { z } from 'zod';

// ---- Schemas (validate everything the daemon returns) ----

export const PingSchema = z.object({
  service: z.string(),
  version: z.string().optional(),
  agent: z.string().optional(),
  auth: z.boolean().optional(),
  addresses: z.array(z.string()).optional(),
  time: z.string().optional(),
});
export type Ping = z.infer<typeof PingSchema>;

export const MessageSchema = z.object({
  role: z.string(),
  content: z.string(),
  created_at: z.string().optional(),
});
export type Message = z.infer<typeof MessageSchema>;

export const MessagesResponseSchema = z.object({
  agent: z.string().optional(),
  messages: z.array(MessageSchema).default([]),
});

export const ChatReplySchema = z.object({
  reply: z.string(),
  agent: z.string().optional(),
});

export class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

function authHeaders(token: string): Record<string, string> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (token) headers.Authorization = `Bearer ${token}`;
  return headers;
}

// ping probes a candidate base URL. Returns the parsed ping (only if it is a
// genuine KARMAX server) or null on any failure/timeout. No auth required.
export async function ping(baseUrl: string, timeoutMs = 2500): Promise<Ping | null> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const res = await fetch(`${baseUrl}/api/ping`, { signal: controller.signal });
    if (!res.ok) return null;
    const parsed = PingSchema.safeParse(await res.json());
    if (!parsed.success || parsed.data.service !== 'karmax') return null;
    return parsed.data;
  } catch {
    return null;
  } finally {
    clearTimeout(timer);
  }
}

export async function fetchMessages(baseUrl: string, token: string, limit = 50): Promise<Message[]> {
  const res = await fetch(`${baseUrl}/api/messages?limit=${limit}`, { headers: authHeaders(token) });
  if (!res.ok) throw new ApiError(`Failed to load messages (${res.status})`, res.status);
  return MessagesResponseSchema.parse(await res.json()).messages;
}

export async function sendChat(baseUrl: string, token: string, message: string): Promise<string> {
  const res = await fetch(`${baseUrl}/api/chat`, {
    method: 'POST',
    headers: authHeaders(token),
    body: JSON.stringify({ message }),
  });
  if (!res.ok) {
    let detail = `Chat failed (${res.status})`;
    try {
      const body = await res.json();
      if (body?.error) detail = String(body.error);
    } catch {
      // ignore non-JSON error bodies
    }
    throw new ApiError(detail, res.status);
  }
  return ChatReplySchema.parse(await res.json()).reply;
}

export async function resetConversation(baseUrl: string, token: string): Promise<void> {
  const res = await fetch(`${baseUrl}/api/conversation/reset`, {
    method: 'POST',
    headers: authHeaders(token),
  });
  if (!res.ok) throw new ApiError(`Reset failed (${res.status})`, res.status);
}

export type CleanupQuestion = {
  done?: boolean;
  memory_id?: string;
  memory?: string;
  question?: string;
  options?: string[];
};

export async function fetchCleanupQuestion(baseUrl: string, token: string): Promise<CleanupQuestion> {
  const res = await fetch(`${baseUrl}/api/memory/cleanup/question`, { headers: authHeaders(token) });
  if (!res.ok) throw new ApiError(`Cleanup failed (${res.status})`, res.status);
  return (await res.json()) as CleanupQuestion;
}

export async function submitCleanupAnswer(
  baseUrl: string,
  token: string,
  body: { memory_id: string; memory: string; question: string; answer: string },
): Promise<void> {
  const res = await fetch(`${baseUrl}/api/memory/cleanup/answer`, {
    method: 'POST',
    headers: authHeaders(token),
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new ApiError(`Answer failed (${res.status})`, res.status);
}

export async function syncContacts(
  baseUrl: string,
  token: string,
  contacts: { name: string; phones: string[] }[],
): Promise<{ synced: number; total: number }> {
  const res = await fetch(`${baseUrl}/api/contacts/sync`, {
    method: 'POST',
    headers: authHeaders(token),
    body: JSON.stringify({ contacts }),
  });
  if (!res.ok) throw new ApiError(`Contacts sync failed (${res.status})`, res.status);
  return (await res.json()) as { synced: number; total: number };
}

export async function fetchContactsCount(baseUrl: string, token: string): Promise<number> {
  const res = await fetch(`${baseUrl}/api/contacts`, { headers: authHeaders(token) });
  if (!res.ok) throw new ApiError(`Contacts status failed (${res.status})`, res.status);
  const j = (await res.json()) as { count?: number };
  return j.count ?? 0;
}

export async function registerPushToken(
  baseUrl: string,
  token: string,
  expoPushToken: string,
  platform: string,
): Promise<void> {
  const res = await fetch(`${baseUrl}/api/push/register`, {
    method: 'POST',
    headers: authHeaders(token),
    body: JSON.stringify({ token: expoPushToken, platform }),
  });
  if (!res.ok) throw new ApiError(`Push registration failed (${res.status})`, res.status);
}

// ---- Proposals (human-in-the-loop approvals) ----

export const ProposalSchema = z.object({
  id: z.string(),
  kind: z.string().optional().default(''),
  title: z.string(),
  summary: z.string().optional().default(''),
  context: z.string().optional().default(''),
  action: z.string().optional().default(''),
  status: z.string(),
  note: z.string().optional().default(''),
  result: z.string().optional().default(''),
  created_at: z.string().optional(),
  decided_at: z.string().optional(),
});
export type Proposal = z.infer<typeof ProposalSchema>;

const ProposalsResponseSchema = z.object({ proposals: z.array(ProposalSchema).default([]) });

export async function fetchProposals(
  baseUrl: string,
  token: string,
  status = 'pending',
): Promise<Proposal[]> {
  const res = await fetch(`${baseUrl}/api/proposals?status=${encodeURIComponent(status)}`, {
    headers: authHeaders(token),
  });
  if (!res.ok) throw new ApiError(`Failed to load approvals (${res.status})`, res.status);
  return ProposalsResponseSchema.parse(await res.json()).proposals;
}

export async function decideProposal(
  baseUrl: string,
  token: string,
  id: string,
  decision: 'approve' | 'reject',
  edit?: string,
  note?: string,
): Promise<Proposal> {
  const res = await fetch(`${baseUrl}/api/proposals/${id}/decision`, {
    method: 'POST',
    headers: authHeaders(token),
    body: JSON.stringify({ decision, edit, note }),
  });
  const json = await res.json().catch(() => ({}) as Record<string, unknown>);
  if (!res.ok) {
    throw new ApiError(String((json as { error?: string }).error ?? `Decision failed (${res.status})`), res.status);
  }
  return ProposalSchema.parse((json as { proposal: unknown }).proposal);
}

// ---- Activity ----

export const JobSchema = z.object({
  id: z.string(),
  name: z.string(),
  cron: z.string().optional().default(''),
  agent: z.string().optional().default(''),
  enabled: z.boolean().optional().default(true),
  run_count: z.number().optional().default(0),
  next_run: z.string().optional(),
  last_run: z.string().optional(),
});
export type Job = z.infer<typeof JobSchema>;

export const ActivitySchema = z.object({
  jobs: z.array(JobSchema).default([]),
  webhooks: z
    .array(z.object({ id: z.string(), route: z.string(), method: z.string(), received_at: z.string().optional() }))
    .default([]),
  coding_sessions: z
    .array(
      z.object({
        id: z.string(),
        tool: z.string().optional().default(''),
        description: z.string().optional().default(''),
        status: z.string().optional().default(''),
        session_id: z.string().optional().default(''),
        updated_at: z.string().optional(),
      }),
    )
    .default([]),
  events: z
    .array(z.object({ id: z.string(), kind: z.string(), agent: z.string().optional().default(''), created_at: z.string().optional() }))
    .default([]),
});
export type Activity = z.infer<typeof ActivitySchema>;

export async function fetchActivity(baseUrl: string, token: string): Promise<Activity> {
  const res = await fetch(`${baseUrl}/api/activity`, { headers: authHeaders(token) });
  if (!res.ok) throw new ApiError(`Failed to load activity (${res.status})`, res.status);
  return ActivitySchema.parse(await res.json());
}

export async function runJob(baseUrl: string, token: string, id: string): Promise<void> {
  const res = await fetch(`${baseUrl}/api/jobs/${id}/run`, { method: 'POST', headers: authHeaders(token) });
  if (!res.ok) throw new ApiError(`Run failed (${res.status})`, res.status);
}

// ---- Memory ----

export const MemoryEntrySchema = z.object({
  id: z.string(),
  role: z.string().optional().default(''),
  content: z.string(),
  tags: z.array(z.string()).optional().default([]),
  created_at: z.string().optional(),
});
export type MemoryEntry = z.infer<typeof MemoryEntrySchema>;

const MemoryEntriesResponseSchema = z.object({
  namespace: z.string().optional(),
  entries: z.array(MemoryEntrySchema).default([]),
});

export type MemTreeNode = {
  node_id?: string;
  title?: string;
  summary?: string;
  content?: string;
  children?: MemTreeNode[];
};

export async function fetchMemoryEntries(baseUrl: string, token: string, q = ''): Promise<MemoryEntry[]> {
  const res = await fetch(`${baseUrl}/api/memory/entries?q=${encodeURIComponent(q)}`, {
    headers: authHeaders(token),
  });
  if (!res.ok) throw new ApiError(`Failed to load memory (${res.status})`, res.status);
  return MemoryEntriesResponseSchema.parse(await res.json()).entries;
}

export async function forgetMemoryEntry(baseUrl: string, token: string, id: string): Promise<void> {
  const res = await fetch(`${baseUrl}/api/memory/entries/${id}`, {
    method: 'DELETE',
    headers: authHeaders(token),
  });
  if (!res.ok) throw new ApiError(`Forget failed (${res.status})`, res.status);
}

export async function fetchMemoryTree(baseUrl: string, token: string): Promise<MemTreeNode | null> {
  const res = await fetch(`${baseUrl}/api/memory/tree`, { headers: authHeaders(token) });
  if (!res.ok) throw new ApiError(`Failed to load tree (${res.status})`, res.status);
  const json = (await res.json()) as { tree?: MemTreeNode | null };
  return json.tree ?? null;
}

export type GraphNode = { id: string; title: string; content?: string; category?: string };
export type GraphLink = { from: string; to: string; relation?: string };
export type MemoryGraph = { nodes: GraphNode[]; links: GraphLink[] };

export async function fetchMemoryGraph(baseUrl: string, token: string): Promise<MemoryGraph> {
  const res = await fetch(`${baseUrl}/api/memory/graph`, { headers: authHeaders(token) });
  if (!res.ok) throw new ApiError(`Graph failed (${res.status})`, res.status);
  const j = (await res.json()) as Partial<MemoryGraph>;
  return { nodes: j.nodes ?? [], links: j.links ?? [] };
}

export async function rebuildMemoryGraph(baseUrl: string, token: string): Promise<MemoryGraph> {
  const res = await fetch(`${baseUrl}/api/memory/graph/rebuild`, {
    method: 'POST',
    headers: authHeaders(token),
  });
  if (!res.ok) throw new ApiError(`Rebuild failed (${res.status})`, res.status);
  const j = (await res.json()) as Partial<MemoryGraph>;
  return { nodes: j.nodes ?? [], links: j.links ?? [] };
}

export async function fetchProfile(baseUrl: string, token: string): Promise<string> {
  const res = await fetch(`${baseUrl}/api/profile`, { headers: authHeaders(token) });
  if (!res.ok) throw new ApiError(`Failed to load profile (${res.status})`, res.status);
  const json = (await res.json()) as { profile?: string };
  return json.profile ?? '';
}

export async function saveProfile(baseUrl: string, token: string, content: string): Promise<void> {
  const res = await fetch(`${baseUrl}/api/profile`, {
    method: 'PUT',
    headers: authHeaders(token),
    body: JSON.stringify({ content }),
  });
  if (!res.ok) throw new ApiError(`Save failed (${res.status})`, res.status);
}

// ---- Device actions (on-device calendar / reminders) ----

export const DeviceActionSchema = z.object({
  id: z.string(),
  kind: z.string(),
  payload: z.record(z.string(), z.unknown()).nullable().optional(),
  status: z.string(),
  result: z.string().optional().default(''),
  created_at: z.string().optional(),
});
export type DeviceAction = z.infer<typeof DeviceActionSchema>;

const DeviceActionsResponseSchema = z.object({ actions: z.array(DeviceActionSchema).default([]) });

export async function fetchDeviceActions(baseUrl: string, token: string, status = ''): Promise<DeviceAction[]> {
  const res = await fetch(`${baseUrl}/api/device/actions?status=${encodeURIComponent(status)}`, {
    headers: authHeaders(token),
  });
  if (!res.ok) throw new ApiError(`Failed to load device actions (${res.status})`, res.status);
  return DeviceActionsResponseSchema.parse(await res.json()).actions;
}

export async function completeDeviceAction(
  baseUrl: string,
  token: string,
  id: string,
  status: string,
  result: string,
): Promise<void> {
  const res = await fetch(`${baseUrl}/api/device/actions/${id}/complete`, {
    method: 'POST',
    headers: authHeaders(token),
    body: JSON.stringify({ status, result }),
  });
  if (!res.ok) throw new ApiError(`Complete failed (${res.status})`, res.status);
}

// ---- Integrations ----

export const IntegrationSchema = z.object({
  id: z.string(),
  name: z.string(),
  status: z.string(),
  detail: z.string().optional().default(''),
});
export type Integration = z.infer<typeof IntegrationSchema>;

const IntegrationsResponseSchema = z.object({ integrations: z.array(IntegrationSchema).default([]) });

export async function fetchIntegrations(baseUrl: string, token: string): Promise<Integration[]> {
  const res = await fetch(`${baseUrl}/api/integrations`, { headers: authHeaders(token) });
  if (!res.ok) throw new ApiError(`Failed to load integrations (${res.status})`, res.status);
  return IntegrationsResponseSchema.parse(await res.json()).integrations;
}
