import { USE_MOCK } from "./config";
import { del, get, post, put } from "./client";
import type { ConsoleUser, ConsoleUsers } from "./types";
import { delay } from "./mock/util";

// Console accounts. Admin-only on the server; the UI mirrors that but never
// relies on it — the check that matters is the one behind the API.

export async function listUsers(): Promise<ConsoleUsers> {
  if (USE_MOCK) {
    return delay({
      users: [{ member: "nikhil", name: "Nikhil", role: "admin", self: true }],
      roles: ["viewer", "operator", "admin"],
    });
  }
  return get<ConsoleUsers>("/api/console/users");
}

export async function createUser(input: {
  member: string; name: string; role: string; password: string;
}): Promise<ConsoleUser> {
  if (USE_MOCK) throw new Error("adding a user needs a real server");
  return post<ConsoleUser>("/api/console/users", input);
}

export async function updateUser(member: string, input: { name?: string; role?: string }): Promise<ConsoleUser> {
  if (USE_MOCK) throw new Error("editing a user needs a real server");
  return put<ConsoleUser>(`/api/console/users/${encodeURIComponent(member)}`, input);
}

export async function deleteUser(member: string): Promise<void> {
  if (USE_MOCK) throw new Error("removing a user needs a real server");
  await del<void>(`/api/console/users/${encodeURIComponent(member)}`);
}

export async function setPassword(
  member: string,
  input: { current_password?: string; password: string },
): Promise<{ sign_in_again: boolean }> {
  if (USE_MOCK) throw new Error("changing a password needs a real server");
  return put<{ sign_in_again: boolean }>(`/api/console/users/${encodeURIComponent(member)}/password`, input);
}
