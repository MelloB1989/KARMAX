import { USE_MOCK } from "./config";
import { get, post } from "./client";
import type { ConnectorHealthCheck, ConnectorSetup, ConnectorSummary } from "./types";
import { CONNECTORS, CONNECTOR_SETUPS } from "./mock/data";
import { delay } from "./mock/util";

export async function listConnectors(): Promise<ConnectorSummary[]> {
  if (USE_MOCK) return delay(CONNECTORS.slice());
  return get<{ connectors: ConnectorSummary[] }>("/api/console/connectors").then((r) => r.connectors);
}

export async function getConnectorSetup(id: string): Promise<ConnectorSetup | null> {
  if (USE_MOCK) return delay(CONNECTOR_SETUPS[id] ?? null);
  return get<ConnectorSetup>(`/api/console/connectors/${encodeURIComponent(id)}/setup`);
}

export async function saveConnectorCredentials(
  id: string,
  fields: Record<string, string>,
): Promise<ConnectorSummary> {
  if (USE_MOCK) {
    const c = CONNECTORS.find((x) => x.id === id);
    if (!c) throw new Error("no such connector: " + id);
    c.status = "healthy";
    c.detail = "Credentials saved — run a health check to confirm";
    return delay(c, 500);
  }
  return post<ConnectorSummary>(`/api/console/connectors/${encodeURIComponent(id)}/credentials`, fields);
}

export async function runConnectorHealthCheck(id: string): Promise<ConnectorHealthCheck> {
  if (USE_MOCK) {
    const c = CONNECTORS.find((x) => x.id === id);
    const result: ConnectorHealthCheck = {
      status: c?.status ?? "not_configured",
      detail: c?.detail ?? "not configured",
      checked_at: new Date().toISOString(),
    };
    if (c) c.last_checked_at = result.checked_at;
    return delay(result, 900);
  }
  return post<ConnectorHealthCheck>(`/api/console/connectors/${encodeURIComponent(id)}/health-check`);
}
