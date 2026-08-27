// Mock-vs-real switch and session storage.
//
// REAL is the default. The backend exists now (internal/api/console*.go), and a
// console that quietly shows invented rows next to real ones is worse than one
// that shows an error — you cannot tell which half you are looking at.
//
// The fixtures survive behind an explicit `?mock=1` so the UI can still be
// demoed with no server attached, which is what they were built for. Anything
// other than that exact opt-in talks to the real API.
//
// The key is versioned: an older visit stored "1" under the previous key and
// would otherwise keep pinning this browser to fixtures forever.
const MOCK_KEY = "ocrew_console_mock_v2";
const TOKEN_KEY = "ocrew_console_token";

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
  typeof window === "undefined" ? false : (localStorage.getItem(MOCK_KEY) ?? "0") === "1";

export function getToken(): string | null {
  if (typeof window === "undefined") return null;
  return localStorage.getItem(TOKEN_KEY);
}

export function setToken(token: string | null): void {
  if (typeof window === "undefined") return;
  if (token) localStorage.setItem(TOKEN_KEY, token);
  else localStorage.removeItem(TOKEN_KEY);
}
