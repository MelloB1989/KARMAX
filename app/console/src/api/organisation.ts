import { USE_MOCK } from "./config";
import { get, put } from "./client";
import type { OrgProfile } from "./types";
import { delay } from "./mock/util";

const EMPTY: OrgProfile = {
  name: "", domain: "", description: "", timezone: "",
  context: "", updated_at: "", updated_by: "", briefing: "",
};

export async function getOrganisation(): Promise<OrgProfile> {
  if (USE_MOCK) return delay(EMPTY);
  return get<OrgProfile>("/api/console/organisation");
}

export async function saveOrganisation(input: Partial<OrgProfile>): Promise<OrgProfile> {
  if (USE_MOCK) throw new Error("saving the organisation needs a real server");
  return put<OrgProfile>("/api/console/organisation", input);
}
