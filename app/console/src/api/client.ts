import { getToken } from "./config";

export class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

// The console is served by the same karmax binary it talks to (see API.md),
// so every real call is same-origin — no base URL to configure.
async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  const token = getToken();
  if (token) headers.Authorization = `Bearer ${token}`;

  const res = await fetch(path, { ...init, headers: { ...headers, ...(init?.headers ?? {}) } });
  if (!res.ok) {
    let detail = `${res.status} ${res.statusText}`;
    try {
      const body = await res.json();
      if (typeof body?.error === "string") detail = body.error;
    } catch {
      /* body wasn't JSON — keep the status line */
    }
    throw new ApiError(detail, res.status);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export function get<T>(path: string): Promise<T> {
  return request<T>(path);
}

export function post<T>(path: string, body?: unknown): Promise<T> {
  return request<T>(path, { method: "POST", body: body === undefined ? undefined : JSON.stringify(body) });
}

export function put<T>(path: string, body?: unknown): Promise<T> {
  return request<T>(path, { method: "PUT", body: body === undefined ? undefined : JSON.stringify(body) });
}
