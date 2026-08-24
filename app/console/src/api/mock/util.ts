// A small fixed delay so the mock UI shows its loading states honestly,
// instead of every request resolving in the same tick real ones never will.
export function delay<T>(value: T, ms = 220): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}
