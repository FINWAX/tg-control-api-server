// Command worker is the data plane: it owns live TDLib clients for the sessions
// it holds, drives login, delivers updates, and claims orphaned sessions from
// the registry. The gateway routes session requests here; workers scale out.
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/zelenin/go-tdlib/client"

	"github.com/FINWAX/tg-control-api-server/internal/api"
	"github.com/FINWAX/tg-control-api-server/internal/secret"
	"github.com/FINWAX/tg-control-api-server/internal/session"
	"github.com/FINWAX/tg-control-api-server/internal/store"
)

func main() {
	// Quiet TDLib's own logging (synchronous, safe before any client exists).
	client.SetLogVerbosityLevel(&client.SetLogVerbosityLevelRequest{NewVerbosityLevel: 1})

	dsn := mustEnv("DATABASE_URL")
	masterKey := mustEnv("MASTER_KEY")
	dataDir := envOr("DATA_DIR", "/data")
	addr := envOr("LISTEN_ADDR", ":8080")
	capacity := envInt("WORKER_CAPACITY", 200)

	sec, err := secret.New(masterKey)
	if err != nil {
		log.Fatalf("secret: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	st, err := store.New(ctx, dsn)
	cancel()
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	workerID := envOr("WORKER_ID", hostname())
	selfAddr := envOr("WORKER_ADVERTISE", "http://"+advertiseIP()+addr)

	mgr := session.NewManager(st, sec, dataDir, workerID, selfAddr, capacity)
	httpSrv := &http.Server{Addr: addr, Handler: api.NewServer(st, sec, mgr)}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		log.Printf("worker %s listening on %s (advertise %s, capacity %d)", workerID, addr, selfAddr, capacity)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	stop() // restore default handler so a second signal force-kills
	log.Printf("worker %s received shutdown signal", workerID)

	shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shCtx) // stop accepting, drain in-flight requests
	mgr.Shutdown(shCtx)         // hand sessions to peers, close clients, deregister
}

// advertiseIP returns this container's routable IP on the compose network. It
// opens a UDP socket toward an off-host address (no packets are sent) and reads
// the local address the kernel would use — the container's bridge IP.
func advertiseIP() string {
	if c, err := net.Dial("udp", "8.8.8.8:53"); err == nil {
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).IP.String()
	}
	return "127.0.0.1"
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "worker"
	}
	return h
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

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
