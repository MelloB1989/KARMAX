export type ThemePref = "light" | "dark" | "system";
const KEY = "karmax_console_theme";

export function getThemePref(): ThemePref {
  if (typeof window === "undefined") return "system";
  return (localStorage.getItem(KEY) as ThemePref | null) ?? "system";
}

// Dark is the unclassed default, so the class marks the exception. An operator
// opening this at 2am should not get a white flash on the way in.
export function applyTheme(pref: ThemePref): void {
  if (typeof document === "undefined") return;
  const light = pref === "light" || (pref === "system" && window.matchMedia("(prefers-color-scheme: light)").matches);
  document.documentElement.classList.toggle("light", light);
}

export function setThemePref(pref: ThemePref): void {
  localStorage.setItem(KEY, pref);
  applyTheme(pref);
}
