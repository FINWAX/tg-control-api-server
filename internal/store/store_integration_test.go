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
	"encoding/json"
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
		          webhook_delivery, update_subscription, sent_message,
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

	// ListSessionsForToken shows exactly what the token may address — it is what
	// a scoped integrator sees instead of the master-only full registry.
	assertVisible(t, st, t1, sA)
	assertVisible(t, st, t2, sA)
	assertVisible(t, st, t3, sA, sB)
	assertVisible(t, st, t4) // disabled: sees nothing

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

// assertVisible checks that ListSessionsForToken returns exactly wantIDs, so the
// listing filter can never drift wider than TokenGrantsSession.
func assertVisible(t *testing.T, st *Store, tokenID string, wantIDs ...string) {
	t.Helper()
	items, err := st.ListSessionsForToken(context.Background(), tokenID)
	if err != nil {
		t.Fatalf("ListSessionsForToken: %v", err)
	}
	got := map[string]bool{}
	for _, s := range items {
		got[s.ID] = true
	}
	if len(got) != len(wantIDs) {
		t.Fatalf("token %s sees %d sessions, want %d", tokenID, len(got), len(wantIDs))
	}
	for _, id := range wantIDs {
		if !got[id] {
			t.Errorf("token %s should see session %s", tokenID, id)
		}
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

func TestMigrationsIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// The baseline migration is recorded, and no version is applied twice.
	var maxVer, rows int
	if err := st.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(version),0), COUNT(*) FROM schema_migration`).Scan(&maxVer, &rows); err != nil {
		t.Fatalf("read schema_migration: %v", err)
	}
	if maxVer != len(migrations) || rows != len(migrations) {
		t.Fatalf("schema_migration = (max %d, rows %d), want %d each", maxVer, rows, len(migrations))
	}

	// Re-running New against the same database applies nothing new and preserves
	// data (an app created before the second run survives).
	appID := mkApp(t, st, "keep")
	st2, err := New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer st2.Close()
	if apps, _ := st2.ListApps(ctx); len(apps) != 1 || apps[0].ID != appID {
		t.Fatalf("data not preserved across re-migration: %+v", apps)
	}
	if err := st2.pool.QueryRow(ctx, `SELECT COUNT(*) FROM schema_migration`).Scan(&rows); err != nil {
		t.Fatalf("recount schema_migration: %v", err)
	}
	if rows != len(migrations) {
		t.Fatalf("schema_migration rows after re-run = %d, want %d", rows, len(migrations))
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

	// routing resolves to the owner's advertised address, and reports the kind
	// so the gateway can validate the {user|bot} path segment
	kind, addr, found, err := st.SessionRoute(ctx, sid, cutoff)
	if err != nil || !found || addr != "http://w1:8080" || kind != "user" {
		t.Fatalf("SessionRoute = (%q, %q, %v, %v)", kind, addr, found, err)
	}

	// releasing makes it an orphan again: still found, but no live owner
	if err := st.ReleaseSession(ctx, sid); err != nil {
		t.Fatalf("ReleaseSession: %v", err)
	}
	if kind, addr, found, _ := st.SessionRoute(ctx, sid, cutoff); addr != "" || !found || kind != "user" {
		t.Fatalf("SessionRoute after release = (%q, %q, %v), want (user, empty, true)", kind, addr, found)
	}

	// an unknown id is distinguishable from an unhosted one (404 vs 503)
	if _, _, found, _ := st.SessionRoute(ctx, "00000000-0000-0000-0000-000000000000", cutoff); found {
		t.Fatal("SessionRoute for an unknown id should report found=false")
	}
}

func TestDeleteSessionCascade(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	appID := mkApp(t, st, "main")
	sid := mkAuthorizedSession(t, st, appID, "bot", "/data/del")

	sub := mkSub(t, st, sid, "", "https://x")
	if err := st.EnqueueDeliveries(ctx, sid, []string{sub}, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("EnqueueDeliveries: %v", err)
	}

	dbDir, found, err := st.DeleteSession(ctx, sid)
	if err != nil || !found || dbDir != "/data/del" {
		t.Fatalf("DeleteSession = (%q, %v, %v)", dbDir, found, err)
	}

	// subscription and deliveries cascaded away
	var subs int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM update_subscription WHERE session_id=$1`, sid).Scan(&subs); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if subs != 0 {
		t.Fatal("subscription survived session delete")
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

// TestSendRegistryOrdering is the point of the registry's upsert shape: the
// worker's registration and TDLib's outcome update race, and either may land
// first. Whichever order they arrive in, the resolved id must win and survive.
func TestSendRegistryOrdering(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	appID := mkApp(t, st, "main")
	sid := mkAuthorizedSession(t, st, appID, "user", "/data/sends")

	// Registration first, outcome second — the ordinary case.
	if err := st.RegisterSend(ctx, sid, -100, 1); err != nil {
		t.Fatalf("RegisterSend: %v", err)
	}
	if rec, found, err := st.GetSend(ctx, sid, -100, 1); err != nil || !found || rec.Status != "pending" {
		t.Fatalf("after register = (%+v, %v, %v), want pending", rec, found, err)
	}
	if err := st.RecordSendOutcome(ctx, sid, -100, 1, 4096, nil); err != nil {
		t.Fatalf("RecordSendOutcome: %v", err)
	}
	rec, found, err := st.GetSend(ctx, sid, -100, 1)
	if err != nil || !found || rec.Status != "sent" || rec.FinalID != 4096 {
		t.Fatalf("after outcome = (%+v, %v, %v), want sent/4096", rec, found, err)
	}

	// Outcome first, registration second — the race. The late registration must
	// not knock a resolved send back to pending.
	if err := st.RecordSendOutcome(ctx, sid, -100, 2, 8192, nil); err != nil {
		t.Fatalf("RecordSendOutcome (early): %v", err)
	}
	if err := st.RegisterSend(ctx, sid, -100, 2); err != nil {
		t.Fatalf("RegisterSend (late): %v", err)
	}
	rec, found, err = st.GetSend(ctx, sid, -100, 2)
	if err != nil || !found || rec.Status != "sent" || rec.FinalID != 8192 {
		t.Fatalf("late register = (%+v, %v, %v), want sent/8192", rec, found, err)
	}

	// A failure carries the error and no final id.
	if err := st.RecordSendOutcome(ctx, sid, -100, 3, 0, []byte(`{"code":400,"message":"NO","source":"tdlib"}`)); err != nil {
		t.Fatalf("RecordSendOutcome (failed): %v", err)
	}
	rec, found, err = st.GetSend(ctx, sid, -100, 3)
	if err != nil || !found || rec.Status != "failed" || rec.FinalID != 0 || len(rec.Error) == 0 {
		t.Fatalf("failed send = (%+v, %v, %v)", rec, found, err)
	}

	// The same temporary id in another chat is a different send: chat_id is in
	// the key because TDLib message ids are unique per chat, not globally.
	if err := st.RegisterSend(ctx, sid, -200, 1); err != nil {
		t.Fatalf("RegisterSend (other chat): %v", err)
	}
	if rec, _, _ := st.GetSend(ctx, sid, -200, 1); rec.Status != "pending" {
		t.Fatalf("other chat = %+v, want its own pending row", rec)
	}

	// Unknown ids are absent, not pending.
	if _, found, err := st.GetSend(ctx, sid, -100, 999); err != nil || found {
		t.Fatalf("unknown send = (%v, %v), want not found", found, err)
	}
}

func TestPurgeSends(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	appID := mkApp(t, st, "main")
	sid := mkAuthorizedSession(t, st, appID, "user", "/data/purge")

	if err := st.RegisterSend(ctx, sid, -100, 1); err != nil {
		t.Fatalf("RegisterSend: %v", err)
	}
	if err := st.RecordSendOutcome(ctx, sid, -100, 2, 4096, nil); err != nil {
		t.Fatalf("RecordSendOutcome: %v", err)
	}

	// Nothing is old enough yet.
	if n, err := st.PurgeSends(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("early purge = (%d, %v), want 0", n, err)
	}
	// Past the cutoff both go, resolved and pending alike.
	if n, err := st.PurgeSends(ctx, time.Now().Add(time.Hour)); err != nil || n != 2 {
		t.Fatalf("purge = (%d, %v), want 2", n, err)
	}
	if _, found, _ := st.GetSend(ctx, sid, -100, 2); found {
		t.Fatal("send survived purge")
	}
}

func TestPendingUserLogin(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	appID := mkApp(t, st, "main")

	if _, found, err := st.PendingUserLogin(ctx, "+15550000"); err != nil || found {
		t.Fatalf("PendingUserLogin on an unknown phone = (%v, %v)", found, err)
	}

	// A fresh session starts in 'connecting', which already counts: creating a
	// second one for the same number would cost another Telegram code.
	sid, err := st.CreateSession(ctx, NewSession{
		Kind: "user", AppID: appID, Phone: "+15550000", DBKeyEnc: []byte("k"),
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got, found, err := st.PendingUserLogin(ctx, "+15550000"); err != nil || !found || got != sid {
		t.Fatalf("PendingUserLogin = (%q, %v, %v), want %q", got, found, err, sid)
	}
	if err := st.UpdateSessionStatus(ctx, sid, "awaiting_password"); err != nil {
		t.Fatalf("UpdateSessionStatus: %v", err)
	}
	if _, found, _ := st.PendingUserLogin(ctx, "+15550000"); !found {
		t.Fatal("a session awaiting a password is still an unfinished login")
	}

	// Once it finishes the number is free again: one number may hold several
	// sessions, so only an unfinished login blocks a new one.
	if err := st.UpdateSessionStatus(ctx, sid, "authorized"); err != nil {
		t.Fatalf("UpdateSessionStatus: %v", err)
	}
	if _, found, _ := st.PendingUserLogin(ctx, "+15550000"); found {
		t.Fatal("an authorized session must not block a new login")
	}

	// Bots have no phone and must never be picked up here.
	if _, found, _ := st.PendingUserLogin(ctx, ""); found {
		t.Fatal("an empty phone must not match")
	}
}

func TestExpireStaleLogins(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	cutoff := time.Now().Add(-30 * time.Second)
	appID := mkApp(t, st, "main")

	mk := func(phone, status, worker string) string {
		id, err := st.CreateSession(ctx, NewSession{
			Kind: "user", AppID: appID, Phone: phone, DBKeyEnc: []byte("k"),
		})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if err := st.UpdateSessionStatus(ctx, id, status); err != nil {
			t.Fatalf("UpdateSessionStatus: %v", err)
		}
		if worker != "" {
			if err := st.SetSessionOwner(ctx, id, worker); err != nil {
				t.Fatalf("SetSessionOwner: %v", err)
			}
		}
		return id
	}

	if err := st.RegisterWorker(ctx, "live", "http://live:8080"); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	held := mk("+1", "awaiting_code", "live")   // owner alive: the flow is reachable
	orphan := mk("+2", "awaiting_code", "")     // nobody can ever deliver the code
	dead := mk("+3", "awaiting_password", "w0") // owner never registered
	fine := mk("+4", "authorized", "live")

	// Nothing is old enough yet: a session mid-creation must survive the sweep.
	if n, err := st.ExpireStaleLogins(ctx, cutoff, time.Hour); err != nil || n != 0 {
		t.Fatalf("ExpireStaleLogins within grace = (%d, %v), want 0", n, err)
	}

	n, err := st.ExpireStaleLogins(ctx, cutoff, 0)
	if err != nil {
		t.Fatalf("ExpireStaleLogins: %v", err)
	}
	if n != 2 {
		t.Fatalf("expired %d logins, want 2 (the ownerless ones)", n)
	}

	want := map[string]string{
		held: "awaiting_code", orphan: "expired", dead: "expired", fine: "authorized",
	}
	for id, expect := range want {
		var got string
		if err := st.pool.QueryRow(ctx, `SELECT status FROM session WHERE id=$1`, id).Scan(&got); err != nil {
			t.Fatalf("read status: %v", err)
		}
		if got != expect {
			t.Errorf("session %s status = %q, want %q", id, got, expect)
		}
	}
}

func mkSub(t *testing.T, st *Store, sessionID, tokenID, url string) string {
	t.Helper()
	id, err := st.CreateSubscription(context.Background(), sessionID, tokenID, url, []byte("sec"), nil)
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	return id
}

func countRows(t *testing.T, st *Store, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", sql, err)
	}
	return n
}

// TestSubscriptionOwnership: a session takes several subscriptions, and a
// scoped token sees and removes only its own while the master token sees all.
func TestSubscriptionOwnership(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	hash := func(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

	appID := mkApp(t, st, "main")
	sid := mkAuthorizedSession(t, st, appID, "bot", "/data/s")
	other := mkAuthorizedSession(t, st, appID, "bot", "/data/o")
	tokA, _ := st.CreateToken(ctx, "a", hash("a"), true, false, nil, []string{sid})
	tokB, _ := st.CreateToken(ctx, "b", hash("b"), true, false, nil, []string{sid})

	master := mkSub(t, st, sid, "", "https://m")
	subA := mkSub(t, st, sid, tokA, "https://a")
	subB := mkSub(t, st, sid, tokB, "https://b")

	all, err := st.ListSubscriptions(ctx, sid, "", "")
	if err != nil || len(all) != 3 {
		t.Fatalf("master list = %d subscriptions (%v), want 3", len(all), err)
	}
	mine, _ := st.ListSubscriptions(ctx, sid, tokA, "")
	if len(mine) != 1 || mine[0].ID != subA || mine[0].TokenID == nil || *mine[0].TokenID != tokA {
		t.Fatalf("token A list = %+v, want only its own", mine)
	}
	if one, _ := st.ListSubscriptions(ctx, sid, tokA, subB); len(one) != 0 {
		t.Fatal("token A could read token B's subscription")
	}
	if one, _ := st.ListSubscriptions(ctx, sid, "", master); len(one) != 1 || one[0].TokenID != nil {
		t.Fatalf("master subscription read = %+v, want one with no owner", one)
	}
	if one, _ := st.ListSubscriptions(ctx, other, "", master); len(one) != 0 {
		t.Fatal("a subscription is readable under another session's path")
	}

	if found, _ := st.DeleteSubscription(ctx, sid, tokA, subB); found {
		t.Fatal("token A removed token B's subscription")
	}
	if found, _ := st.DeleteSubscription(ctx, sid, tokA, master); found {
		t.Fatal("token A removed the master's subscription")
	}
	if found, _ := st.DeleteSubscription(ctx, sid, tokA, "not-a-uuid"); found {
		t.Fatal("a malformed id matched something")
	}
	if found, err := st.DeleteSubscription(ctx, sid, tokA, subA); err != nil || !found {
		t.Fatalf("token A removing its own = (%v, %v), want found", found, err)
	}
	if found, _ := st.DeleteSubscription(ctx, sid, "", subB); !found {
		t.Fatal("master could not remove a token's subscription")
	}

	// Filters round-trip through jsonb, and the worker's view lists them.
	f, err := st.CreateSubscription(ctx, sid, "", "https://f", nil, []byte(`{"types":["updateNewMessage"],"chats":[-100]}`))
	if err != nil {
		t.Fatalf("CreateSubscription with filters: %v", err)
	}
	views, err := st.SubscriptionFilters(ctx, []string{sid, other})
	if err != nil || len(views) != 2 {
		t.Fatalf("SubscriptionFilters = %d (%v), want 2 (master + filtered)", len(views), err)
	}
	var spec struct {
		Types []string `json:"types"`
		Chats []int64  `json:"chats"`
	}
	if err := json.Unmarshal(views[1].Filters, &spec); err != nil || views[1].ID != f ||
		len(spec.Types) != 1 || spec.Types[0] != "updateNewMessage" || len(spec.Chats) != 1 || spec.Chats[0] != -100 {
		t.Fatalf("filters came back as %s (%v)", views[1].Filters, err)
	}
	if views[0].Filters != nil {
		t.Fatalf("an unfiltered subscription reported filters %s", views[0].Filters)
	}

	// Queueing targets only subscriptions that still exist, on this session.
	theirs := mkSub(t, st, other, "", "https://t")
	if err := st.EnqueueDeliveries(ctx, sid, []string{master, subA, theirs}, []byte(`{"x":1}`)); err != nil {
		t.Fatalf("EnqueueDeliveries: %v", err)
	}
	if n := countRows(t, st, `SELECT count(*) FROM webhook_delivery`); n != 1 {
		t.Fatalf("queued %d rows, want 1 (the removed and the foreign subscription skipped)", n)
	}
}

// TestClaimDeliveries covers what decides which rows a worker sends: its own
// sessions only, a cap per subscription, subscriptions already in flight left
// out, and a token's access re-checked at every claim.
func TestClaimDeliveries(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	hash := func(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

	appA := mkApp(t, st, "A")
	appB := mkApp(t, st, "B")
	sid := mkAuthorizedSession(t, st, appA, "bot", "/data/a")
	foreign := mkAuthorizedSession(t, st, appB, "user", "/data/f")
	for _, w := range []string{"w1", "w2"} {
		if err := st.RegisterWorker(ctx, w, "http://"+w); err != nil {
			t.Fatalf("RegisterWorker: %v", err)
		}
	}
	_ = st.SetSessionOwner(ctx, sid, "w1")
	_ = st.SetSessionOwner(ctx, foreign, "w2")

	tok, _ := st.CreateToken(ctx, "t", hash("t"), true, false, nil, []string{sid})
	master := mkSub(t, st, sid, "", "https://m")
	scoped := mkSub(t, st, sid, tok, "https://s")
	theirs := mkSub(t, st, foreign, "", "https://o")
	for i := 0; i < 3; i++ {
		if err := st.EnqueueDeliveries(ctx, sid, []string{master, scoped}, []byte(`{"n":1}`)); err != nil {
			t.Fatalf("EnqueueDeliveries: %v", err)
		}
	}
	_ = st.EnqueueDeliveries(ctx, foreign, []string{theirs}, []byte(`{"n":2}`))

	claim := func(busy []string, perSub int) map[string]int {
		t.Helper()
		ds, err := st.ClaimDeliveries(ctx, "w1", busy, perSub, 100)
		if err != nil {
			t.Fatalf("ClaimDeliveries: %v", err)
		}
		got := map[string]int{}
		for _, d := range ds {
			if d.SessionID != sid || d.Kind != "bot" || d.URL == "" {
				t.Fatalf("claimed %+v, want a row of w1's bot session with its target", d)
			}
			got[d.SubscriptionID]++
		}
		return got
	}
	same := func(name string, got, want map[string]int) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: claimed %v, want %v", name, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s: claimed %v, want %v", name, got, want)
			}
		}
	}

	same("own sessions only", claim(nil, 10), map[string]int{master: 3, scoped: 3})
	same("capped per subscription", claim(nil, 2), map[string]int{master: 2, scoped: 2})
	same("busy subscription skipped", claim([]string{master}, 10), map[string]int{scoped: 3})

	off, on := false, true
	_, _ = st.UpdateToken(ctx, tok, TokenPatch{Enabled: &off})
	same("disabled token", claim(nil, 10), map[string]int{master: 3})
	_, _ = st.UpdateToken(ctx, tok, TokenPatch{Enabled: &on})

	none := []string{}
	_, _ = st.UpdateToken(ctx, tok, TokenPatch{SessionIDs: &none})
	same("session out of scope", claim(nil, 10), map[string]int{master: 3})
	apps := []string{appA}
	_, _ = st.UpdateToken(ctx, tok, TokenPatch{AppIDs: &apps})
	same("scope back through the app", claim(nil, 10), map[string]int{master: 3, scoped: 3})

	// A deleted token takes its subscription, and what it had queued, along.
	if found, _ := st.DeleteToken(ctx, tok); !found {
		t.Fatal("DeleteToken: not found")
	}
	if n := countRows(t, st, `SELECT count(*) FROM update_subscription WHERE id=$1`, scoped); n != 0 {
		t.Fatal("subscription survived its token")
	}
	if n := countRows(t, st, `SELECT count(*) FROM webhook_delivery WHERE subscription_id=$1`, scoped); n != 0 {
		t.Fatalf("%d deliveries survived their subscription", n)
	}
}

// TestDeliveryOutcomeStatus: each outcome lands on the row and on the
// subscription's status, which also counts what is pending, failed and lost.
func TestDeliveryOutcomeStatus(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	appID := mkApp(t, st, "main")
	sid := mkAuthorizedSession(t, st, appID, "user", "/data/s")
	_ = st.RegisterWorker(ctx, "w1", "http://w1")
	_ = st.SetSessionOwner(ctx, sid, "w1")
	sub := mkSub(t, st, sid, "", "https://r")
	sibling := mkSub(t, st, sid, "", "https://r2")
	for i := 0; i < 3; i++ {
		_ = st.EnqueueDeliveries(ctx, sid, []string{sub}, []byte(`{"n":1}`))
	}
	ds, err := st.ClaimDeliveries(ctx, "w1", nil, 10, 10)
	if err != nil || len(ds) != 3 {
		t.Fatalf("ClaimDeliveries = %d rows (%v), want 3", len(ds), err)
	}

	if err := st.MarkDelivered(ctx, ds[0].ID); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	if err := st.FailDelivery(ctx, ds[1].ID, time.Now().Add(time.Hour), false, "status 500"); err != nil {
		t.Fatalf("FailDelivery (retry): %v", err)
	}
	if err := st.FailDelivery(ctx, ds[2].ID, time.Time{}, true, "timeout"); err != nil {
		t.Fatalf("FailDelivery (give up): %v", err)
	}
	if err := st.AddDropped(ctx, sid, 4); err != nil {
		t.Fatalf("AddDropped: %v", err)
	}

	got, _ := st.ListSubscriptions(ctx, sid, "", sub)
	if len(got) != 1 {
		t.Fatalf("subscription not found")
	}
	s := got[0]
	if s.LastDeliveredAt == nil || s.LastError == nil || *s.LastError != "timeout" || s.LastErrorAt == nil {
		t.Fatalf("status = %+v, want last success and last error 'timeout'", s)
	}
	if s.Pending != 1 || s.Failed != 1 || s.Dropped != 4 {
		t.Fatalf("counts = pending %d failed %d dropped %d, want 1/1/4", s.Pending, s.Failed, s.Dropped)
	}
	// A loss happens before routing, so every subscription of the session is charged.
	if sib, _ := st.ListSubscriptions(ctx, sid, "", sibling); sib[0].Dropped != 4 {
		t.Fatalf("sibling dropped = %d, want 4", sib[0].Dropped)
	}

	var attempts int
	if err := st.pool.QueryRow(ctx, `SELECT attempts FROM webhook_delivery WHERE id=$1`, ds[1].ID).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("retried row attempts = %d (%v), want 1", attempts, err)
	}
	// The retry is scheduled an hour out, the others are done: nothing is due.
	if again, _ := st.ClaimDeliveries(ctx, "w1", nil, 10, 10); len(again) != 0 {
		t.Fatalf("claimed %d rows, want none due", len(again))
	}
}

// TestPurgeDeliveriesStalePending: rows past retention go whatever their
// status, so a queue that can no longer be sent does not grow forever.
func TestPurgeDeliveriesStalePending(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	appID := mkApp(t, st, "main")
	sid := mkAuthorizedSession(t, st, appID, "bot", "/data/s")
	sub := mkSub(t, st, sid, "", "https://r")
	_ = st.EnqueueDeliveries(ctx, sid, []string{sub}, []byte(`{"old":1}`))
	if _, err := st.pool.Exec(ctx, `UPDATE webhook_delivery SET created_at = now() - interval '48 hours'`); err != nil {
		t.Fatalf("age row: %v", err)
	}
	_ = st.EnqueueDeliveries(ctx, sid, []string{sub}, []byte(`{"new":1}`))

	n, err := st.PurgeDeliveries(ctx, time.Now().Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("PurgeDeliveries = (%d, %v), want 1 (the stale pending row)", n, err)
	}
	if left := countRows(t, st, `SELECT count(*) FROM webhook_delivery WHERE status='pending'`); left != 1 {
		t.Fatalf("%d pending rows left, want the fresh one", left)
	}
}

// TestMigrationV3Backfill runs the real upgrade path: a database at v2, with a
// subscription and rows queued the old way, is migrated to v3. Queued rows must
// land on the subscription they were meant for, rows of a session without one
// are dropped (nothing could ever send them), and a second subscription per
// session becomes possible.
func TestMigrationV3Backfill(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	full := migrations
	t.Cleanup(func() { migrations = full })
	migrate := func(upTo int) {
		t.Helper()
		migrations = full[:upTo]
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		if err := applyMigrations(ctx, tx); err != nil {
			t.Fatalf("migrate to %d: %v", upTo, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	if _, err := st.pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	migrate(2)

	var appID, withSub, withoutSub, legacy string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO tg_app (api_id, api_hash_enc) VALUES (1, 'x') RETURNING id`).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	for _, dst := range []*string{&withSub, &withoutSub} {
		if err := st.pool.QueryRow(ctx,
			`INSERT INTO session (kind, app_id) VALUES ('bot', $1) RETURNING id`, appID).Scan(dst); err != nil {
			t.Fatalf("seed session: %v", err)
		}
	}
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO update_subscription (session_id, url) VALUES ($1, 'https://old') RETURNING id`,
		withSub).Scan(&legacy); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	for _, sess := range []string{withSub, withSub, withoutSub} {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO webhook_delivery (session_id, payload) VALUES ($1, '{}')`, sess); err != nil {
			t.Fatalf("seed delivery: %v", err)
		}
	}

	migrate(len(full))

	if n := countRows(t, st, `SELECT count(*) FROM webhook_delivery WHERE subscription_id=$1`, legacy); n != 2 {
		t.Fatalf("%d queued rows attributed to the legacy subscription, want 2", n)
	}
	if n := countRows(t, st, `SELECT count(*) FROM webhook_delivery`); n != 2 {
		t.Fatalf("%d rows after backfill, want 2 (the subscription-less one dropped)", n)
	}
	subs, err := st.ListSubscriptions(ctx, withSub, "", "")
	if err != nil || len(subs) != 1 || subs[0].TokenID != nil || subs[0].Pending != 2 || subs[0].Dropped != 0 {
		t.Fatalf("legacy subscription after upgrade = %+v (%v), want master-owned with 2 pending", subs, err)
	}
	if _, err := st.CreateSubscription(ctx, withSub, "", "https://second", nil, nil); err != nil {
		t.Fatalf("second subscription on one session refused: %v", err)
	}
}

func TestSessionLimits(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sid := mkAuthorizedSession(t, st, mkApp(t, st, "a"), "user", "/d/limits")

	l, found, err := st.SessionLimits(ctx, sid)
	if err != nil || !found {
		t.Fatalf("SessionLimits: found %v err %v", found, err)
	}
	if l.IsPremium != nil || l.Frozen || l.SpamLimited || len(l.Cooldowns) != 0 {
		t.Errorf("fresh session = %+v, want nothing known", l)
	}

	now := time.Now()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.SetCooldown(ctx, sid, Cooldown{Scope: "@resolve", Until: now.Add(time.Hour), Reason: "long"}))
	must(st.SetCooldown(ctx, sid, Cooldown{Scope: "@resolve", Until: now.Add(time.Minute), Reason: "short"}))
	must(st.SetCooldown(ctx, sid, Cooldown{Scope: SpamLimitScope, Until: now.Add(24 * time.Hour), Reason: "PEER_FLOOD"}))
	must(st.SetCooldown(ctx, sid, Cooldown{Scope: "@join", Until: now.Add(-time.Minute), Reason: "over"}))
	must(st.SetSessionPremium(ctx, sid, true))
	must(st.SetSessionFrozen(ctx, sid, true))

	l, _, err = st.SessionLimits(ctx, sid)
	must(err)
	if l.IsPremium == nil || !*l.IsPremium || !l.Frozen || l.FrozenSince == nil || !l.SpamLimited {
		t.Errorf("flags = %+v", l)
	}
	if len(l.Cooldowns) != 2 || l.Cooldowns[0].Scope != "@resolve" || l.Cooldowns[0].Reason != "long" {
		t.Errorf("cooldowns = %+v, want @resolve (the longer one) then spam_limit", l.Cooldowns)
	}

	// The listing carries the same flags.
	items, err := st.ListSessions(ctx)
	must(err)
	if len(items) != 1 || items[0].IsPremium == nil || !*items[0].IsPremium || !items[0].Frozen || !items[0].SpamLimited {
		t.Errorf("listing = %+v", items)
	}

	// Freezing again keeps the first sighting; clearing drops it.
	first := *l.FrozenSince
	must(st.SetSessionFrozen(ctx, sid, true))
	l, _, _ = st.SessionLimits(ctx, sid)
	if !l.FrozenSince.Equal(first) {
		t.Errorf("frozen_since moved: %v -> %v", first, l.FrozenSince)
	}
	must(st.SetSessionFrozen(ctx, sid, false))
	l, _, _ = st.SessionLimits(ctx, sid)
	if l.Frozen || l.FrozenSince != nil {
		t.Errorf("unfrozen = %+v", l)
	}

	// Telegram's own report sets its dates and link; clearing drops them too.
	since, deletion := now.Add(-time.Hour).Truncate(time.Second), now.Add(30*24*time.Hour).Truncate(time.Second)
	must(st.SetSessionFreeze(ctx, sid, since, deletion, "https://t.me/spambot"))
	l, _, _ = st.SessionLimits(ctx, sid)
	if !l.Frozen || !l.FrozenSince.Equal(since) || l.DeletionDate == nil || !l.DeletionDate.Equal(deletion) || l.AppealLink != "https://t.me/spambot" {
		t.Errorf("reported freeze = %+v", l)
	}
	must(st.SetSessionFrozen(ctx, sid, true)) // a FROZEN_* error keeps the report
	l, _, _ = st.SessionLimits(ctx, sid)
	if !l.FrozenSince.Equal(since) || l.DeletionDate == nil || l.AppealLink == "" {
		t.Errorf("report lost to a FROZEN_* sighting: %+v", l)
	}
	must(st.SetSessionFrozen(ctx, sid, false))
	l, _, _ = st.SessionLimits(ctx, sid)
	if l.Frozen || l.DeletionDate != nil || l.AppealLink != "" {
		t.Errorf("unfrozen after report = %+v", l)
	}

	if found, err := st.DeleteCooldown(ctx, sid, "@resolve"); err != nil || !found {
		t.Errorf("DeleteCooldown = %v, %v; want found", found, err)
	}
	if found, _ := st.DeleteCooldown(ctx, sid, "@resolve"); found {
		t.Error("DeleteCooldown found a cooldown already lifted")
	}
	if n, err := st.PurgeCooldowns(ctx, now); err != nil || n != 1 {
		t.Errorf("PurgeCooldowns = %d, %v; want the one ended", n, err)
	}
	if _, found, _ := st.SessionLimits(ctx, "00000000-0000-0000-0000-000000000000"); found {
		t.Error("unknown session found")
	}
	if _, _, err := st.DeleteSession(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, `SELECT count(*) FROM cooldown`); n != 0 {
		t.Errorf("cooldowns outlived their session: %d", n)
	}
}

func TestLimitsAndCounters(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sid := mkAuthorizedSession(t, st, mkApp(t, st, "a"), "user", "/d/quota")
	kind := "user"

	if _, err := st.CreateLimit(ctx, Limit{Level: "session", SessionID: ptr("00000000-0000-0000-0000-000000000000"),
		Methods: []string{"@join"}, Max: 1, Per: "day", Enabled: true}); err != ErrLimitTarget {
		t.Errorf("unknown session: err %v, want ErrLimitTarget", err)
	}
	def, err := st.CreateLimit(ctx, Limit{Level: "session", Kind: &kind, Methods: []string{"@join"}, Max: 3, Per: "day", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	own, err := st.CreateLimit(ctx, Limit{Level: "session", SessionID: &sid, Methods: []string{"@send"}, Max: 5, Per: "hour", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now().Truncate(time.Hour)
	use := func(l *Limit, n int) CounterUse {
		return CounterUse{Key: CounterKey{LimitID: l.ID, Bucket: sid, WindowStart: start}, N: n, Max: l.Max}
	}
	take := func(uses ...CounterUse) int {
		t.Helper()
		i, err := st.TakeCounters(ctx, uses)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	counts := func() (int, int) {
		t.Helper()
		c, err := st.Counters(ctx, []CounterKey{use(def, 0).Key, use(own, 0).Key})
		if err != nil {
			t.Fatal(err)
		}
		return c[use(def, 0).Key], c[use(own, 0).Key]
	}

	if i := take(use(def, 2), use(own, 4)); i != -1 {
		t.Fatalf("first take refused at %d", i)
	}
	// The second use would overfill: neither is counted.
	if i := take(use(def, 1), use(own, 2)); i != 1 {
		t.Errorf("overfill refused at %d, want 1", i)
	}
	if d, o := counts(); d != 2 || o != 4 {
		t.Errorf("after refusal: %d, %d; want 2, 4 (all or nothing)", d, o)
	}
	// A single use larger than the whole window never fits.
	if i := take(use(def, 4)); i != 0 {
		t.Errorf("oversized use refused at %d, want 0", i)
	}
	if i := take(use(def, 1)); i != -1 {
		t.Errorf("exact fit refused at %d", i)
	}
	if err := st.GiveBackCounters(ctx, []CounterUse{use(def, 2), use(own, 9)}); err != nil {
		t.Fatal(err)
	}
	if d, o := counts(); d != 1 || o != 0 {
		t.Errorf("after give back: %d, %d; want 1, 0 (never below zero)", d, o)
	}
	if n, err := st.MaxCounter(ctx, def.ID, start); err != nil || n != 1 {
		t.Errorf("MaxCounter = %d, %v", n, err)
	}

	off := false
	own.Max, own.Enabled = 50, off
	if u, err := st.UpdateLimit(ctx, *own); err != nil || u.Max != 50 || u.Enabled {
		t.Errorf("UpdateLimit = %+v, %v", u, err)
	}
	if l, err := st.GetLimit(ctx, "00000000-0000-0000-0000-000000000000"); l != nil || err != nil {
		t.Errorf("GetLimit unknown = %v, %v", l, err)
	}
	if n, err := st.PurgeCounters(ctx, start.Add(time.Second)); err != nil || n != 2 {
		t.Errorf("PurgeCounters = %d, %v; want 2", n, err)
	}

	// Removing the session takes its own rules with it, and their counters.
	take(use(own, 1))
	if _, _, err := st.DeleteSession(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if l, _ := st.GetLimit(ctx, own.ID); l != nil {
		t.Error("session's own rule outlived it")
	}
	if found, err := st.DeleteLimit(ctx, def.ID); err != nil || !found {
		t.Errorf("DeleteLimit = %v, %v", found, err)
	}
	if n := countRows(t, st, `SELECT count(*) FROM rate_counter`); n != 0 {
		t.Errorf("counters left: %d", n)
	}
}

func ptr(s string) *string { return &s }
