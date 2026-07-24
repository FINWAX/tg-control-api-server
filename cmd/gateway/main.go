// Command gateway is the control plane: a stateless HTTP front that holds no
// TDLib clients. It reads the Postgres registry to route each request to the
// worker that owns the target session, and reverse-proxies to it.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/FINWAX/tg-control-api-server/internal/obs"
	"github.com/FINWAX/tg-control-api-server/internal/router"
	"github.com/FINWAX/tg-control-api-server/internal/store"
)

func main() {
	dsn := mustEnv("DATABASE_URL")
	token := mustEnv("API_TOKEN") // fail closed: no token, no gateway
	addr := envOr("LISTEN_ADDR", ":8080")

	shutdownObs, err := obs.Setup(context.Background(), "tg-control-api-server-gateway")
	if err != nil {
		log.Fatalf("obs: %v", err)
	}

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
	httpSrv := &http.Server{Addr: addr, Handler: otelhttp.NewHandler(srv, "gateway")}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		log.Printf("gateway listening on %s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	stop()
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shCtx) // stop accepting, drain in-flight
	_ = shutdownObs(shCtx)      // flush telemetry
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
