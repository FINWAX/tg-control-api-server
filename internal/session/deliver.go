// Durable update delivery (at-least-once, unordered). Updates from the TDLib
// receiver goroutine are handed to an in-memory queue; a writer matches each
// against the session's subscriptions and persists one webhook_delivery row per
// match; a dispatcher POSTs them with retry/backoff. Persisting first decouples
// delivery from the receiver (which must never block) and lets delivery survive
// restarts and a temporarily-down consumer. Rows are per subscription, so each
// receiver's retries and failures are its own.
package session

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FINWAX/tg-control-api-server/internal/store"
)

// outItem is one update on its way to the queue, with the session's
// subscriptions as they stood when it arrived.
type outItem struct {
	sessionID string
	ctor      string
	payload   []byte
	subs      *subSnapshot
}

const (
	maxDeliveryAttempts = 10
	backoffBase         = 5 * time.Second
	backoffCap          = 5 * time.Minute
	deliveryTimeout     = 10 * time.Second

	// A receiver is posted to by one goroutine at a time, at most perSubBatch
	// rows per round (one round per tick, so this is also its rate ceiling);
	// up to maxInflightSubs receivers are served at once.
	maxInflightSubs = 16
	perSubBatch     = 100
	maxErrorLen     = 500 // longest error kept on a subscription

	retentionAge      = 24 * time.Hour   // keep terminal rows this long
	retentionInterval = 10 * time.Minute // sweep cadence
	deadWorkerAge     = time.Hour        // drop registry rows unseen this long
)

// runRetention periodically purges old delivered/failed rows so the outbox
// table doesn't grow without bound. It sweeps once at startup, then on a timer.
func (m *Manager) runRetention() {
	m.purgeOnce()
	tick := time.NewTicker(retentionInterval)
	defer tick.Stop()
	for range tick.C {
		m.purgeOnce()
	}
}

func (m *Manager) purgeOnce() {
	ctx := context.Background()
	n, err := m.st.PurgeDeliveries(ctx, time.Now().Add(-retentionAge))
	if err != nil {
		log.Printf("retention: %v", err)
	} else if n > 0 {
		log.Printf("retention: purged %d terminal deliveries", n)
	}
	if w, err := m.st.PurgeWorkers(ctx, time.Now().Add(-deadWorkerAge)); err != nil {
		log.Printf("retention: workers: %v", err)
	} else if w > 0 {
		log.Printf("retention: purged %d dead workers", w)
	}
	if n, err := m.st.PurgeSends(ctx, time.Now().Add(-m.sendTTL)); err != nil {
		log.Printf("retention: sends: %v", err)
	} else if n > 0 {
		log.Printf("retention: purged %d send records", n)
	}
	m.sweepUploads()
}

// runOutboxWriter drains the in-memory queue into webhook_delivery, one row
// per subscription the update matches.
func (m *Manager) runOutboxWriter() {
	ctx := context.Background()
	for it := range m.outbox {
		ids := it.subs.route(it.ctor, it.payload)
		if len(ids) == 0 {
			continue
		}
		if err := m.st.EnqueueDeliveries(ctx, it.sessionID, ids, it.payload); err != nil {
			// As lost as a full queue, and reported the same way.
			if ls := m.get(it.sessionID); ls != nil {
				ls.dropped.Add(1)
			}
			log.Printf("outbox: enqueue %s: %v", it.sessionID, err)
		}
	}
}

// runOutboxDispatcher delivers due rows, retrying with exponential backoff.
// Each subscription's rows go out in order on a goroutine of their own, and a
// subscription still busy is not claimed again, so a receiver that hangs until
// the timeout delays only itself. The dispatcher never waits for a round to
// finish: rows of a busy or over-quota receiver simply stay due.
func (m *Manager) runOutboxDispatcher() {
	ctx := context.Background()
	httpc := &http.Client{Timeout: deliveryTimeout}
	var (
		mu       sync.Mutex
		inflight = map[string]bool{}
	)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for range tick.C {
		mu.Lock()
		busy := make([]string, 0, len(inflight))
		for id := range inflight {
			busy = append(busy, id)
		}
		mu.Unlock()
		free := maxInflightSubs - len(busy)
		if free <= 0 {
			continue
		}
		batch, err := m.st.ClaimDeliveries(ctx, m.workerID, busy, perSubBatch, free*perSubBatch)
		if err != nil {
			log.Printf("outbox: claim: %v", err)
			continue
		}
		groups, order := groupBySubscription(batch)
		if len(order) > free {
			order = order[:free]
		}
		for _, sub := range order {
			rows := groups[sub]
			mu.Lock()
			inflight[sub] = true
			mu.Unlock()
			go func() {
				defer func() {
					mu.Lock()
					delete(inflight, sub)
					mu.Unlock()
				}()
				// A failure most likely means the receiver is down: stop
				// there rather than wait out the timeout on every row.
				for _, d := range rows {
					if !m.deliverOne(ctx, httpc, d) {
						return
					}
				}
			}()
		}
	}
}

// groupBySubscription splits a claimed batch per subscription, keeping claim
// order within each and listing subscriptions by their first row.
func groupBySubscription(batch []store.Delivery) (map[string][]store.Delivery, []string) {
	groups := map[string][]store.Delivery{}
	var order []string
	for _, d := range batch {
		if _, seen := groups[d.SubscriptionID]; !seen {
			order = append(order, d.SubscriptionID)
		}
		groups[d.SubscriptionID] = append(groups[d.SubscriptionID], d)
	}
	return groups, order
}

// deliverOne posts one row and records the outcome, reporting whether the
// receiver accepted it.
func (m *Manager) deliverOne(ctx context.Context, httpc *http.Client, d store.Delivery) bool {
	err := m.post(httpc, d)
	if err == nil {
		_ = m.st.MarkDelivered(ctx, d.ID)
		return true
	}
	giveUp := d.Attempts+1 >= maxDeliveryAttempts
	backoff := time.Duration(math.Min(
		float64(backoffCap), float64(backoffBase)*math.Pow(2, float64(d.Attempts))))
	reason := err.Error()
	if len(reason) > maxErrorLen {
		// Cut on a rune boundary: Postgres rejects invalid UTF-8, and a failed
		// write here would leave the row due, retried every tick uncounted.
		reason = strings.ToValidUTF8(reason[:maxErrorLen], "")
	}
	_ = m.st.FailDelivery(ctx, d.ID, time.Now().Add(backoff), giveUp, reason)
	if giveUp {
		log.Printf("outbox: giving up on %s after %d attempts: %v", d.ID, d.Attempts+1, err)
	}
	return false
}

func (m *Manager) post(httpc *http.Client, d store.Delivery) error {
	req, err := http.NewRequest(http.MethodPost, d.URL, bytes.NewReader(d.Payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Who this is for and which attempt. The delivery id is the row's, so a
	// retry carries the same one and the receiver can deduplicate on it. These
	// are not covered by the signature — with a secret per subscription, the
	// key that verifies is what proves where a delivery came from.
	req.Header.Set("X-Tgcontrol-Session", d.SessionID)
	req.Header.Set("X-Tgcontrol-Kind", d.Kind)
	req.Header.Set("X-Tgcontrol-Subscription", d.SubscriptionID)
	req.Header.Set("X-Tgcontrol-Delivery", d.ID)
	req.Header.Set("X-Tgcontrol-Attempt", strconv.Itoa(d.Attempts+1))
	if len(d.SecretEnc) > 0 {
		secret, err := m.sec.Decrypt(d.SecretEnc)
		if err != nil {
			return err
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write(d.Payload)
		req.Header.Set("X-Tgcontrol-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
