import { USE_MOCK, getToken, setToken } from "./config";
import { get, post } from "./client";
import type { BootstrapStatus, Session } from "./types";
import { delay } from "./mock/util";

// A single-org, self-hosted console: no signup, no org switching. The very
// first admin is created once via bootstrap(); after that only login() works.
const MOCK_BOOTSTRAPPED_KEY = "ocrew_console_mock_bootstrapped";

export async function bootstrapStatus(): Promise<BootstrapStatus> {
  if (USE_MOCK) {
    const done = localStorage.getItem(MOCK_BOOTSTRAPPED_KEY) === "1";
    return delay({ needs_bootstrap: !done });
  }
  return get<BootstrapStatus>("/api/console/auth/bootstrap-status");
}

export async function bootstrap(input: { name: string; member: string; password: string }): Promise<Session> {
  if (USE_MOCK) {
    localStorage.setItem(MOCK_BOOTSTRAPPED_KEY, "1");
    const session: Session = { token: "mock-session-token", member: input.member, name: input.name, role: "admin" };
    setToken(session.token);
    return delay(session, 400);
  }
  const session = await post<Session>("/api/console/auth/bootstrap", input);
  setToken(session.token);
  return session;
}

export async function login(input: { member: string; password: string }): Promise<Session> {
  if (USE_MOCK) {
    const session: Session = { token: "mock-session-token", member: input.member, name: input.member, role: "admin" };
    setToken(session.token);
    return delay(session, 400);
  }
  const session = await post<Session>("/api/console/auth/login", input);
  setToken(session.token);
  return session;
}

export async function me(): Promise<Session | null> {
  if (USE_MOCK) {
    const token = getToken();
    if (!token) return delay(null);
    return delay({ token, member: "nikhil", name: "Nikhil", role: "admin" });
  }
  try {
    return await get<Session>("/api/console/auth/me");
  } catch {
    return null;
  }
}

export function logout(): void {
  setToken(null);
  if (!USE_MOCK) void post("/api/console/auth/logout").catch(() => {});
}
