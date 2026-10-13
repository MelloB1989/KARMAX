// Command fakegithub serves an in-memory GitHub for exercising the org office
// locally. Point org_ops.github.api_url at it and it at KARMAX's /orgops/github:
//
//	fakegithub -addr :8787 -webhook http://127.0.0.1:9090/orgops/github -secret dev -seed acme
//
// Then drive it:
//
//	curl -XPOST localhost:8787/_fake/pulls -d '{"repo":"acme/payments-api","title":"feat: x","author":"dev1",
//	     "files":[{"filename":"main.go","patch":"@@ -1,1 +1,2 @@\n x\n+key := \"AKIAABCDEFGHIJKLMNOP\""}]}'
//	curl -XPOST localhost:8787/_fake/ci -d '{"repo":"acme/payments-api","number":1,"state":"success"}'
//	curl localhost:8787/_fake/state
package main

import (
	"flag"
	"log"
	"net/http"
	"strings"

	"github.com/MelloB1989/karmax/internal/orgops/fakegithub"
)

func main() {
	addr := flag.String("addr", ":8787", "listen address")
	public := flag.String("public", "", "URL this server is reached at (for html_url); default http://localhost<addr>")
	webhook := flag.String("webhook", "", "KARMAX webhook URL to deliver events to (…/orgops/github)")
	secret := flag.String("secret", "", "webhook secret (org_ops.github.webhook_secret)")
	seed := flag.String("seed", "", "seed a demo org with this name (template + two services)")
	flag.Parse()

	base := *public
	if base == "" {
		base = "http://" + *addr
		if strings.HasPrefix(*addr, ":") {
			base = "http://localhost" + *addr
		}
	}
	s := fakegithub.New(base)
	s.Webhook, s.Secret = *webhook, *secret
	if *seed != "" {
		fakegithub.Seed(s, *seed)
	}
	log.Printf("fake GitHub on %s (webhooks -> %q)", *addr, *webhook)
	log.Fatal(http.ListenAndServe(*addr, s))
}
