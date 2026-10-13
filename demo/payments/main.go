// Demo payments service: a tiny stdlib HTTP server with a fault switch, so the
// org office's incident autopilot has something real to probe.
//
// Fault mode simulates a bad deploy: the refund handler ships without its
// nil-customer guard, so refunding a guest-checkout charge (no customer)
// dereferences nil and panics. The panic is recovered into a 500, and while the
// fault is on /health and /charges report the failure too, which is what the
// probe sees. The fix PR (demo/fakefixer.sh) adds the guard to this handler.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

type Customer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Charge.Customer is nil for guest checkouts.
type Charge struct {
	ID       string    `json:"id"`
	Customer *Customer `json:"customer,omitempty"`
	Amount   int64     `json:"amount"`
	Currency string    `json:"currency"`
	Status   string    `json:"status"`
}

var charges = []*Charge{
	{ID: "ch_1001", Customer: &Customer{"cus_maya", "Maya"}, Amount: 4200, Currency: "usd", Status: "succeeded"},
	{ID: "ch_1002", Customer: &Customer{"cus_raj", "Raj"}, Amount: 1850, Currency: "usd", Status: "succeeded"},
	{ID: "ch_1003", Customer: &Customer{"cus_ana", "Ana"}, Amount: 990, Currency: "usd", Status: "refunded"},
	{ID: "ch_1004", Customer: nil, Amount: 2500, Currency: "usd", Status: "succeeded"}, // guest checkout
}

func find(id string) *Charge {
	for _, c := range charges {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// recoverer turns a handler panic into a 500, as net/http would, but logs it.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, rec)
				reply(w, 500, map[string]string{"error": "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// latency makes the service feel real on a dashboard: 20-60 ms normally, with a
// slow tail (about 1 in 25 requests takes 800-1200 ms). The fault path pays it too.
func latency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := 20 + rand.Intn(41)
		if rand.Intn(25) == 0 {
			d = 800 + rand.Intn(401)
		}
		time.Sleep(time.Duration(d) * time.Millisecond)
		next.ServeHTTP(w, r)
	})
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9494", "listen address")
	flag.Parse()

	var fault atomic.Bool
	fault.Store(os.Getenv("FAULT") == "1")

	log.Printf("payments listening on %s (fault=%v)", *addr, fault.Load())
	log.Fatal(http.ListenAndServe(*addr, newHandler(&fault)))
}

func newHandler(fault *atomic.Bool) http.Handler {
	// While the bad deploy is live, every endpoint reports the outage.
	healthy := func(ok http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if fault.Load() {
				reply(w, 500, map[string]string{"error": "nil customer in refund handler"})
				return
			}
			ok(w, r)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthy(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, map[string]string{"status": "ok"})
	}))
	mux.HandleFunc("GET /charges", healthy(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, charges)
	}))
	mux.HandleFunc("POST /charges/{id}/refund", func(w http.ResponseWriter, r *http.Request) {
		charge := find(r.PathValue("id"))
		if charge == nil {
			reply(w, 404, map[string]string{"error": "charge not found"})
			return
		}
		if !fault.Load() && charge.Customer == nil { // the guard the bad deploy dropped
			reply(w, 422, map[string]string{"error": "charge has no customer"})
			return
		}
		name := charge.Customer.Name // nil dereference for guest charges when the guard is gone
		reply(w, 200, map[string]any{"refunded": charge.Amount, "currency": charge.Currency, "receipt_to": name})
	})
	mux.HandleFunc("POST /fault/on", func(w http.ResponseWriter, _ *http.Request) {
		fault.Store(true)
		reply(w, 200, map[string]bool{"fault": true})
	})
	mux.HandleFunc("POST /fault/off", func(w http.ResponseWriter, _ *http.Request) {
		fault.Store(false)
		reply(w, 200, map[string]bool{"fault": false})
	})

	return recoverer(latency(mux))
}
