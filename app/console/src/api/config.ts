// Mock-vs-real switch and session storage.
//
// Mock is the default: the backend in API.md doesn't exist yet, and this
// console has to be demoable on its own. `?mock=0` (once, from any page)
// flips it to real API calls and remembers the choice; `?mock=1` flips back.
const MOCK_KEY = "karmax_console_mock";
const TOKEN_KEY = "karmax_console_token";

function readOverride(): void {
  if (typeof window === "undefined") return;
  const params = new URLSearchParams(window.location.search);
  const override = params.get("mock");
  if (override === "0" || override === "1") {
    localStorage.setItem(MOCK_KEY, override);
  }
}
readOverride();

export const USE_MOCK: boolean =
  typeof window === "undefined" ? true : (localStorage.getItem(MOCK_KEY) ?? "1") !== "0";

export function getToken(): string | null {
  if (typeof window === "undefined") return null;
  return localStorage.getItem(TOKEN_KEY);
}

export function setToken(token: string | null): void {
  if (typeof window === "undefined") return;
  if (token) localStorage.setItem(TOKEN_KEY, token);
  else localStorage.removeItem(TOKEN_KEY);
}
