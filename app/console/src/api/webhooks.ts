import { USE_MOCK } from "./config";
import { del, get, post, put } from "./client";
import type { WebhookCatalogue, WebhookDelivery, WebhookRow } from "./types";
import { delay } from "./mock/util";

export async function listWebhooks(): Promise<{ webhooks: WebhookRow[]; base_url: string }> {
  if (USE_MOCK) return delay({ webhooks: [], base_url: "" });
  return get<{ webhooks: WebhookRow[]; base_url: string }>("/api/console/webhooks");
}

export async function listDeliveries(endpoint?: string): Promise<WebhookDelivery[]> {
  if (USE_MOCK) return delay([]);
  const q = endpoint ? `?endpoint=${encodeURIComponent(endpoint)}` : "";
  const r = await get<{ deliveries: WebhookDelivery[] }>(`/api/console/webhooks/deliveries${q}`);
  return r.deliveries;
}

/** Everything KARMAX supports, so the form can offer it instead of asking. */
export async function getCatalogue(): Promise<WebhookCatalogue> {
  if (USE_MOCK) return delay({ platforms: [], signature_headers: [], agents: [], base_url: "" });
  return get<WebhookCatalogue>("/api/console/webhooks/catalogue");
}

export async function createWebhook(input: {
  slug: string; name: string; description?: string; platform: string; event_kind?: string;
  secret?: string; signature_header?: string; agent_id?: string; enabled: boolean;
}): Promise<WebhookRow> {
  if (USE_MOCK) throw new Error("creating a webhook needs a real server");
  return post<WebhookRow>("/api/console/webhooks", input);
}

/** Only the fields present are changed; the rest are left alone. */
export async function updateWebhook(id: string, input: Partial<{
  slug: string; name: string; description: string; event_kind: string;
  secret: string; signature_header: string; agent_id: string; enabled: boolean;
}>): Promise<WebhookRow> {
  if (USE_MOCK) throw new Error("editing a webhook needs a real server");
  return put<WebhookRow>(`/api/console/webhooks/${encodeURIComponent(id)}`, input);
}

export async function deleteWebhook(id: string): Promise<void> {
  if (USE_MOCK) throw new Error("deleting a webhook needs a real server");
  await del<void>(`/api/console/webhooks/${encodeURIComponent(id)}`);
}
