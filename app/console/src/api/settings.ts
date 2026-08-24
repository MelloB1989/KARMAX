import { USE_MOCK } from "./config";
import { get, post, put } from "./client";
import type { DirectorySyncStatus, OrgRole, OrgRoleName, Settings } from "./types";
import { DIRECTORY_SYNC, MODEL_PROVIDERS, ORG_ROLES, SANDBOX_TOKEN } from "./mock/data";
import { delay } from "./mock/util";

export async function getSettings(): Promise<Settings> {
  if (USE_MOCK) {
    return delay({
      model_providers: MODEL_PROVIDERS.slice(),
      sandbox_token: SANDBOX_TOKEN,
      directory: DIRECTORY_SYNC,
      roles: ORG_ROLES.slice(),
    });
  }
  return get<Settings>("/api/console/settings");
}

export async function saveModelProvider(
  id: string,
  input: { base_url: string; api_key?: string },
): Promise<void> {
  if (USE_MOCK) {
    const p = MODEL_PROVIDERS.find((x) => x.id === id);
    if (p) {
      p.base_url = input.base_url;
      if (input.api_key) {
        p.has_key = true;
        p.key_last4 = input.api_key.slice(-4);
      }
    }
    await delay(undefined, 400);
    return;
  }
  await put(`/api/console/settings/model/${encodeURIComponent(id)}`, input);
}

export async function saveSandboxToken(token: string): Promise<void> {
  if (USE_MOCK) {
    SANDBOX_TOKEN.configured = true;
    SANDBOX_TOKEN.last4 = token.slice(-4);
    SANDBOX_TOKEN.updated_at = new Date().toISOString();
    await delay(undefined, 400);
    return;
  }
  await put("/api/console/settings/sandbox-token", { token });
}

export async function syncDirectory(): Promise<DirectorySyncStatus> {
  if (USE_MOCK) {
    DIRECTORY_SYNC.last_synced_at = new Date().toISOString();
    return delay(DIRECTORY_SYNC, 900);
  }
  return post<DirectorySyncStatus>("/api/console/settings/directory/sync");
}

export async function setOrgRole(member: string, role: OrgRoleName): Promise<OrgRole> {
  if (USE_MOCK) {
    const r = ORG_ROLES.find((x) => x.member === member);
    if (!r) throw new Error("no such member: " + member);
    r.role = role;
    return delay(r, 300);
  }
  return put<OrgRole>(`/api/console/settings/roles/${encodeURIComponent(member)}`, { role });
}
