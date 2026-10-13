#!/usr/bin/env bash
# Simulated engineer for the offline demo. KARMAX calls it like fleetctl:
#   fakefixer.sh run - --repo R --task inc-xxxxx --from A --wait --timeout T   (brief on stdin)
# The fake GitHub runs no git, so this opens the fix PR through its control API.
set -euo pipefail
GH=${FAKE_GITHUB:-http://127.0.0.1:8787}
PAY=${PAYMENTS_URL:-http://127.0.0.1:9494}

repo="" task=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo) repo=$2; shift 2 ;;
    --task) task=$2; shift 2 ;;
    *) shift ;;
  esac
done
brief=$(cat)
[ -n "$repo" ] || repo=acme/payments-api
case "$repo" in */*) ;; *) repo=acme/$repo ;; esac
task=${task:-inc-demo}
echo "fakefixer: brief received ($(printf %s "$brief" | wc -c) bytes) for $repo / $task" >&2

fix_patch='@@ -41,6 +41,9 @@ func (h *Handler) Refund(w http.ResponseWriter, r *http.Request) {
 	charge, err := h.store.Charge(r.Context(), chargeID)
 	if err != nil {
 		http.Error(w, "charge not found", http.StatusNotFound)
 		return
 	}
+	if charge.Customer == nil {
+		http.Error(w, "charge has no customer", http.StatusUnprocessableEntity)
+		return
+	}
-	name := charge.Customer.Name
+	name := charge.Customer.Name // safe: guarded above'
test_patch='@@ -0,0 +1,18 @@
+package payments
+
+import (
+	"net/http"
+	"net/http/httptest"
+	"testing"
+)
+
+func TestRefundWithoutCustomerIsRejected(t *testing.T) {
+	h := newTestHandler(t, Charge{ID: "ch_1003", Customer: nil})
+	rec := httptest.NewRecorder()
+	h.Refund(rec, httptest.NewRequest(http.MethodPost, "/charges/ch_1003/refund", nil))
+	if rec.Code != http.StatusUnprocessableEntity {
+		t.Fatalf("want 422, got %d", rec.Code)
+	}
+}'
body="Incident ${task}

**Root cause:** the refund handler dereferenced \`charge.Customer\` without a nil check; charges created by guest checkout have no customer, so every refund of one panicked into a 500 and /health began failing.

**Fix:** guard the nil customer and answer 422 instead of crashing.

**Tested:** added TestRefundWithoutCustomerIsRejected; go test ./... passes."

payload=$(jq -n --arg repo "$repo" --arg head "karmax/fix-$task" --arg body "$body" \
  --arg fp "$fix_patch" --arg tp "$test_patch" '{
  repo:$repo, head:$head, author:"karmax-fixer",
  title:"fix: guard nil customer in refund handler", body:$body,
  files:[{filename:"internal/payments/refund.go",patch:$fp},
         {filename:"internal/payments/refund_test.go",status:"added",patch:$tp}]}')
sleep 3
pr=$(curl -fsS -XPOST "$GH/_fake/pulls" -d "$payload")
n=$(printf %s "$pr" | jq -r .number)
echo "fakefixer: opened PR #$n on branch karmax/fix-$task" >&2

sleep 5
curl -fsS -XPOST "$GH/_fake/ci" -d "{\"repo\":\"$repo\",\"number\":$n,\"state\":\"success\"}" >/dev/null
echo "fakefixer: CI green; 'deploying' the fix" >&2
curl -fsS -XPOST "$PAY/fault/off" >/dev/null
sleep "${FIXER_WAIT:-20}"

jq -cn --arg url "$GH/$repo/pull/$n" '{pr_url:$url,
  root_cause:"Refund handler dereferenced a nil customer on guest-checkout charges, panicking into 500s.",
  summary:"Added a nil-customer guard returning 422 in the refund handler, plus a regression test."}' \
  | sed 's/^/KARMAX_RESULT /'
