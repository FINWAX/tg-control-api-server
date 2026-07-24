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
	"strconv"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/FINWAX/tg-control-api-server/internal/obs"
	"github.com/FINWAX/tg-control-api-server/internal/router"
	"github.com/FINWAX/tg-control-api-server/internal/store"
	"github.com/FINWAX/tg-control-api-server/internal/upload"
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

	// Shared uploads volume, mounted read-write here and on the workers that
	// read the files back via inputFileLocal.
	uploads := upload.New(
		envOr("UPLOADS_DIR", "/uploads"),
		envInt64("MAX_SINGLE_SHOT_BYTES", 64<<20), // 64 MiB single-shot
		envInt64("MAX_CHUNK_BYTES", 16<<20),       // 16 MiB per resumable chunk
		envInt64("MAX_UPLOAD_BYTES", 2<<30),       // 2 GiB total (Telegram's ceiling)
	)

	// A worker is routable while its heartbeat is within this window (matches
	// the worker's own staleness threshold).
	srv := router.New(st, 30*time.Second, token, uploads)
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

func envInt64(k string, def int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
