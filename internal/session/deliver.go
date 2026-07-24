// Durable update delivery (at-least-once). Updates from the TDLib receiver
// goroutine are handed to an in-memory queue, persisted to webhook_delivery by
// a writer, then POSTed by a dispatcher with retry/backoff. Persisting first
// decouples delivery from the receiver (which must never block) and lets
// delivery survive restarts and a temporarily-down consumer.
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
	"time"

	"github.com/FINWAX/tg-control-api-server/internal/store"
)

type outItem struct {
	sessionID string
	payload   []byte
}

const (
	maxDeliveryAttempts = 10
	backoffBase         = 5 * time.Second
	backoffCap          = 5 * time.Minute

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
}

// runOutboxWriter drains the in-memory queue into webhook_delivery.
func (m *Manager) runOutboxWriter() {
	ctx := context.Background()
	for it := range m.outbox {
		if err := m.st.EnqueueDelivery(ctx, it.sessionID, it.payload); err != nil {
			log.Printf("outbox: enqueue %s: %v", it.sessionID, err)
		}
	}
}

// runOutboxDispatcher delivers due rows, retrying with exponential backoff.
func (m *Manager) runOutboxDispatcher() {
	ctx := context.Background()
	httpc := &http.Client{Timeout: 10 * time.Second}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for range tick.C {
		batch, err := m.st.ClaimDeliveries(ctx, m.workerID, 50)
		if err != nil {
			log.Printf("outbox: claim: %v", err)
			continue
		}
		for _, d := range batch {
			m.deliverOne(ctx, httpc, d)
		}
	}
}

func (m *Manager) deliverOne(ctx context.Context, httpc *http.Client, d store.Delivery) {
	err := m.post(httpc, d)
	if err == nil {
		_ = m.st.MarkDelivered(ctx, d.ID)
		return
	}
	giveUp := d.Attempts+1 >= maxDeliveryAttempts
	backoff := time.Duration(math.Min(
		float64(backoffCap), float64(backoffBase)*math.Pow(2, float64(d.Attempts))))
	_ = m.st.FailDelivery(ctx, d.ID, time.Now().Add(backoff), giveUp)
	if giveUp {
		log.Printf("outbox: giving up on %s after %d attempts: %v", d.ID, d.Attempts+1, err)
	}
}

func (m *Manager) post(httpc *http.Client, d store.Delivery) error {
	req, err := http.NewRequest(http.MethodPost, d.URL, bytes.NewReader(d.Payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
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
