package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	addr := os.Getenv("TEAM_DEMO_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	// Task: add GET /api/users according to the shared contract.
	log.Fatal(http.ListenAndServe(addr, mux))
}
