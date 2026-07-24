//go:build integration

// Integration tests for the Postgres-backed store. They need a real database
// (DATABASE_URL) and are excluded from the default `go test` build by the
// `integration` tag. Run them with scripts/test-integration.sh, which spins up
// a throwaway Postgres. Being in package store, they may touch st.pool directly
// for setup/reset.
package store

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)
	reset(t, st)
	return st
}

// reset clears all tables so each test starts clean (CASCADE handles FKs).
func reset(t *testing.T, st *Store) {
	t.Helper()
	_, err := st.pool.Exec(context.Background(),
		`TRUNCATE api_token, api_token_app, api_token_session,
		          webhook_delivery, update_subscription,
		          session, proxy, tg_app, worker RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
}

// mkApp / mkAuthorizedSession are setup helpers.
func mkApp(t *testing.T, st *Store, label string) string {
	t.Helper()
	id, err := st.CreateApp(context.Background(), 12345, []byte("enc"), label)
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return id
}

func mkAuthorizedSession(t *testing.T, st *Store, appID, kind, dbDir string) string {
	t.Helper()
	ctx := context.Background()
	id, err := st.CreateSession(ctx, NewSession{Kind: kind, AppID: appID, DBKeyEnc: []byte("dbkey")})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := st.SetSessionDBDir(ctx, id, dbDir); err != nil {
		t.Fatalf("SetSessionDBDir: %v", err)
	}
	if err := st.UpdateSessionStatus(ctx, id, "authorized"); err != nil {
		t.Fatalf("UpdateSessionStatus: %v", err)
	}
	return id
}

func TestCRUDAndListings(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	appID := mkApp(t, st, "main")
	proxyID, err := st.CreateProxy(ctx, Proxy{Type: "socks5", Host: "h", Port: 1080, Username: "u", SecretEnc: []byte("s"), Label: "p"})
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	sid := mkAuthorizedSession(t, st, appID, "bot", "/data/x")
	if err := st.SetSessionOwner(ctx, sid, "w1"); err != nil {
		t.Fatalf("SetSessionOwner: %v", err)
	}

	if apps, _ := st.ListApps(ctx); len(apps) != 1 || apps[0].Label != "main" {
		t.Fatalf("ListApps = %+v", apps)
	}
	if px, _ := st.ListProxies(ctx); len(px) != 1 || px[0].Host != "h" {
		t.Fatalf("ListProxies = %+v", px)
	}
	sessions, _ := st.ListSessions(ctx)
	if len(sessions) != 1 || sessions[0].AppLabel != "main" {
		t.Fatalf("ListSessions = %+v", sessions)
	}

	// rename paths
	if ok, _ := st.UpdateAppLabel(ctx, appID, "renamed"); !ok {
		t.Fatal("UpdateAppLabel not found")
	}
	if ok, _ := st.UpdateProxyLabel(ctx, proxyID, "px2"); !ok {
		t.Fatal("UpdateProxyLabel not found")
	}
	newLabel := "botlabel"
	if ok, _ := st.UpdateSessionMeta(ctx, sid, &newLabel, nil); !ok {
		t.Fatal("UpdateSessionMeta not found")
	}
	apps, _ := st.ListApps(ctx)
	if apps[0].Label != "renamed" {
		t.Fatalf("rename app failed: %+v", apps[0])
	}
}

func TestTokenScope(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	appA := mkApp(t, st, "A")
	appB := mkApp(t, st, "B")
	sA := mkAuthorizedSession(t, st, appA, "bot", "/data/a")
	sB := mkAuthorizedSession(t, st, appB, "bot", "/data/b")

	hash := func(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

	// token1: explicit session sA only
	t1, err := st.CreateToken(ctx, "sess", hash("t1"), true, false, nil, []string{sA})
	if err != nil {
		t.Fatalf("CreateToken t1: %v", err)
	}
	assertGrant(t, st, t1, sA, true)
	assertGrant(t, st, t1, sB, false)

	// token2: all sessions of app A
	t2, _ := st.CreateToken(ctx, "app", hash("t2"), true, false, []string{appA}, nil)
	assertGrant(t, st, t2, sA, true)
	assertGrant(t, st, t2, sB, false)

	// token3: all_sessions
	t3, _ := st.CreateToken(ctx, "all", hash("t3"), true, true, nil, nil)
	assertGrant(t, st, t3, sA, true)
	assertGrant(t, st, t3, sB, true)

	// token4: disabled -> grants nothing despite matching scope
	t4, _ := st.CreateToken(ctx, "off", hash("t4"), false, true, nil, nil)
	assertGrant(t, st, t4, sA, false)

	// ResolveToken by hash
	id, enabled, found, err := st.ResolveToken(ctx, hash("t1"))
	if err != nil || !found || !enabled || id != t1 {
		t.Fatalf("ResolveToken t1 = (%q,%v,%v,%v)", id, enabled, found, err)
	}
	if _, _, found, _ := st.ResolveToken(ctx, hash("nope")); found {
		t.Fatal("ResolveToken unknown should be not found")
	}

	// ListTokens returns scope without secrets
	toks, _ := st.ListTokens(ctx)
	if len(toks) != 4 {
		t.Fatalf("ListTokens count = %d", len(toks))
	}

	// UpdateToken: flip t1 to app-B scope, drop session grant
	enable := true
	if ok, _ := st.UpdateToken(ctx, t1, TokenPatch{Enabled: &enable, AppIDs: &[]string{appB}, SessionIDs: &[]string{}}); !ok {
		t.Fatal("UpdateToken not found")
	}
	assertGrant(t, st, t1, sA, false)
	assertGrant(t, st, t1, sB, true)

	// DeleteToken
	if ok, _ := st.DeleteToken(ctx, t1); !ok {
		t.Fatal("DeleteToken not found")
	}
	if _, _, found, _ := st.ResolveToken(ctx, hash("t1")); found {
		t.Fatal("token still resolvable after delete")
	}
}

func assertGrant(t *testing.T, st *Store, tokenID, sessionID string, want bool) {
	t.Helper()
	got, err := st.TokenGrantsSession(context.Background(), tokenID, sessionID)
	if err != nil {
		t.Fatalf("TokenGrantsSession: %v", err)
	}
	if got != want {
		t.Errorf("TokenGrantsSession(%s, %s) = %v, want %v", tokenID, sessionID, got, want)
	}
}

func TestOwnershipClaimAndRoute(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	cutoff := time.Now().Add(-30 * time.Second)

	appID := mkApp(t, st, "main")
	sid := mkAuthorizedSession(t, st, appID, "user", "/data/u") // orphan (no worker_id)

	if err := st.RegisterWorker(ctx, "w1", "http://w1:8080"); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}

	claimed, err := st.ClaimOrphans(ctx, "w1", cutoff, 10)
	if err != nil {
		t.Fatalf("ClaimOrphans: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != sid {
		t.Fatalf("ClaimOrphans = %+v", claimed)
	}

	// a second claim finds nothing (already owned by a live worker)
	if again, _ := st.ClaimOrphans(ctx, "w2", cutoff, 10); len(again) != 0 {
		t.Fatalf("second ClaimOrphans should be empty, got %+v", again)
	}

	// routing resolves to the owner's advertised address
	addr, err := st.SessionRoute(ctx, sid, cutoff)
	if err != nil || addr != "http://w1:8080" {
		t.Fatalf("SessionRoute = (%q, %v)", addr, err)
	}

	// releasing makes it an orphan again
	if err := st.ReleaseSession(ctx, sid); err != nil {
		t.Fatalf("ReleaseSession: %v", err)
	}
	if addr, _ := st.SessionRoute(ctx, sid, cutoff); addr != "" {
		t.Fatalf("SessionRoute after release = %q, want empty", addr)
	}
}

func TestDeleteSessionCascade(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	appID := mkApp(t, st, "main")
	sid := mkAuthorizedSession(t, st, appID, "bot", "/data/del")

	if err := st.SetWebhook(ctx, sid, "https://x", []byte("sec"), nil); err != nil {
		t.Fatalf("SetWebhook: %v", err)
	}
	if err := st.EnqueueDelivery(ctx, sid, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("EnqueueDelivery: %v", err)
	}

	dbDir, found, err := st.DeleteSession(ctx, sid)
	if err != nil || !found || dbDir != "/data/del" {
		t.Fatalf("DeleteSession = (%q, %v, %v)", dbDir, found, err)
	}

	// subscription and deliveries cascaded away
	if wh, _ := st.GetWebhook(ctx, sid); wh != nil {
		t.Fatal("webhook subscription survived session delete")
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM webhook_delivery WHERE session_id=$1`, sid).Scan(&n); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if n != 0 {
		t.Fatalf("deliveries survived cascade: %d", n)
	}

	// deleting again reports not found
	if _, found, _ := st.DeleteSession(ctx, sid); found {
		t.Fatal("second DeleteSession should be not found")
	}
}
