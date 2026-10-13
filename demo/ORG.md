# Acme engineering rules

- No secrets in code: keys, tokens and passwords come from the environment.
- Pull request titles follow conventional commits (`feat:`, `fix:`, `chore:` …).
- Every change ships with tests that can fail.
- Money is integer minor units, never float.
- Every handler validates input and returns typed errors.
