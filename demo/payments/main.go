// Demo payments service: a tiny stdlib HTTP server with a fault switch, so the
// org office's incident autopilot has something real to probe.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"sync/atomic"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9494", "listen address")
	flag.Parse()

	var fault atomic.Bool
	fault.Store(os.Getenv("FAULT") == "1")

	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	guard := func(ok func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if fault.Load() {
				reply(w, 500, map[string]string{"error": "nil customer in refund handler"})
				return
			}
			ok(w, r)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", guard(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, map[string]string{"status": "ok"})
	}))
	mux.HandleFunc("GET /charges", guard(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, []map[string]any{
			{"id": "ch_1001", "customer": "cus_maya", "amount": 4200, "currency": "usd", "status": "succeeded"},
			{"id": "ch_1002", "customer": "cus_raj", "amount": 1850, "currency": "usd", "status": "succeeded"},
			{"id": "ch_1003", "customer": "cus_ana", "amount": 990, "currency": "usd", "status": "refunded"},
		})
	}))
	mux.HandleFunc("POST /fault/on", func(w http.ResponseWriter, _ *http.Request) {
		fault.Store(true)
		reply(w, 200, map[string]bool{"fault": true})
	})
	mux.HandleFunc("POST /fault/off", func(w http.ResponseWriter, _ *http.Request) {
		fault.Store(false)
		reply(w, 200, map[string]bool{"fault": false})
	})

	log.Printf("payments listening on %s (fault=%v)", *addr, fault.Load())
	log.Fatal(http.ListenAndServe(*addr, mux))
}
