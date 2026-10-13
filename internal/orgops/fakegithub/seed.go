package fakegithub

// Seed creates a small demo org: a service template and two services with
// docs and rules, enough for reviews, indexing and repo creation to have
// something real to read.
func Seed(s *Server, org string) {
	s.AddRepo(org+"/template-go-service", "Go service template", map[string]string{
		"README.md":                "# Go service template\n\nHTTP service skeleton with health checks, structured logging and CI.\n",
		"main.go":                  "package main\n\nfunc main() {}\n",
		".github/workflows/ci.yml": "name: ci\non: [pull_request]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - run: go test ./...\n",
	})
	s.AddRepo(org+"/payments-api", "Payments API: charges, refunds and payouts", map[string]string{
		"README.md":  "# payments-api\n\nCharges, refunds and payouts over REST. Go 1.22, Postgres, Stripe.\n\nRun: `go run .` · Test: `go test ./...`\n\nDepends on auth-service for JWT validation.\n",
		"CLAUDE.md":  "# Rules for payments-api\n\n- Money is integer minor units, never float.\n- Every handler validates input and returns typed errors.\n- No secrets in code; read them from the environment.\n",
		"CODEOWNERS": "* @payments-team\n",
	})
	s.AddRepo(org+"/auth-service", "Auth service: login, sessions and JWT issuing", map[string]string{
		"README.md":  "# auth-service\n\nIssues and validates JWTs (RS256). Node 20 + Redis sessions.\n\nAPIs: POST /login, POST /refresh, GET /.well-known/jwks.json\n",
		"CODEOWNERS": "* @identity-team\n",
	})
}
