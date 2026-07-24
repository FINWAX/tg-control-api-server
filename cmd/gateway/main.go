// Command gateway is the control plane: a stateless HTTP front that holds no
// TDLib clients. It reads the Postgres registry to route each request to the
// worker that owns the target session, and reverse-proxies to it.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/FINWAX/tg-control-api-server/internal/router"
	"github.com/FINWAX/tg-control-api-server/internal/store"
)

func main() {
	dsn := mustEnv("DATABASE_URL")
	token := mustEnv("API_TOKEN") // fail closed: no token, no gateway
	addr := envOr("LISTEN_ADDR", ":8080")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	st, err := store.New(ctx, dsn)
	cancel()
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	// A worker is routable while its heartbeat is within this window (matches
	// the worker's own staleness threshold).
	srv := router.New(st, 30*time.Second, token)

	log.Printf("gateway listening on %s", addr)
	if err := http.ListenAndServe(addr, srv); err != nil {
		log.Fatal(err)
	}
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s is required", k)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
