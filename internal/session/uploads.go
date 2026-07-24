package session

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/zelenin/go-tdlib/client"

	"github.com/FINWAX/tg-control-api-server/internal/upload"
)

// ErrLocalPathDenied is returned when a /call references an inputFileLocal path
// outside the uploads volume — a guard against reading arbitrary worker files.
var ErrLocalPathDenied = errors.New("inputFileLocal path must be within the uploads volume")

// guardLocalPaths scans a call's params for inputFileLocal file paths, rejects
// any outside the uploads volume, and returns the distinct upload directories
// referenced (so they can be cleaned up once the send completes).
func (m *Manager) guardLocalPaths(params json.RawMessage) ([]string, error) {
	paths := collectLocalFilePaths(params)
	if len(paths) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	var dirs []string
	for _, p := range paths {
		if !upload.Within(p, m.uploadsDir) {
			return nil, ErrLocalPathDenied
		}
		d := filepath.Dir(p)
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	return dirs, nil
}

// collectLocalFilePaths walks arbitrary td_api params and returns the "path" of
// every {"@type":"inputFileLocal", "path": "..."} object, at any nesting depth.
func collectLocalFilePaths(params json.RawMessage) []string {
	var v any
	if len(params) == 0 || json.Unmarshal(params, &v) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case map[string]any:
			if t["@type"] == "inputFileLocal" {
				if p, ok := t["path"].(string); ok {
					out = append(out, p)
				}
			}
			for _, c := range t {
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(v)
	return out
}

// trackUpload remembers, keyed by the temporary message id(s) in a send result,
// which upload dirs to remove once TDLib confirms the send. Results without a
// message id (e.g. setProfilePhoto) aren't tracked here — the TTL sweep covers
// them.
func (m *Manager) trackUpload(res json.RawMessage, dirs []string) {
	ids := extractMessageIDs(res)
	if len(ids) == 0 {
		return
	}
	m.pendMu.Lock()
	for _, id := range ids {
		m.pending[id] = dirs
	}
	m.pendMu.Unlock()
}

// extractMessageIDs pulls temporary message ids from a send result — a single
// `message` object or a `messages` array (sendMessageAlbum).
func extractMessageIDs(res json.RawMessage) []int64 {
	var obj struct {
		Type     string `json:"@type"`
		ID       int64  `json:"id"`
		Messages []struct {
			ID int64 `json:"id"`
		} `json:"messages"`
	}
	if json.Unmarshal(res, &obj) != nil {
		return nil
	}
	if obj.Type == "message" && obj.ID != 0 {
		return []int64{obj.ID}
	}
	var ids []int64
	for _, mm := range obj.Messages {
		if mm.ID != 0 {
			ids = append(ids, mm.ID)
		}
	}
	return ids
}

// finishUpload removes the upload dirs tied to a completed send. It reads the
// old (temporary) message id from an updateMessageSendSucceeded/Failed update.
func (m *Manager) finishUpload(update client.Type) {
	b, err := json.Marshal(update)
	if err != nil {
		return
	}
	var u struct {
		OldMessageID int64 `json:"old_message_id"`
	}
	if json.Unmarshal(b, &u) != nil || u.OldMessageID == 0 {
		return
	}
	m.pendMu.Lock()
	dirs := m.pending[u.OldMessageID]
	delete(m.pending, u.OldMessageID)
	m.pendMu.Unlock()
	for _, d := range dirs {
		if err := os.RemoveAll(d); err != nil {
			log.Printf("upload cleanup %s: %v", d, err)
		}
	}
}

// sweepUploads removes upload dirs older than the TTL — the safety net for
// uploads that were never sent, non-message uploads, or a completion missed
// because the worker restarted (pending map is in-memory).
func (m *Manager) sweepUploads() {
	if m.uploadsDir == "" {
		return
	}
	n, err := m.uploads.Sweep(time.Now().Add(-m.uploadTTL))
	if err != nil {
		log.Printf("upload sweep: %v", err)
	} else if n > 0 {
		log.Printf("upload sweep: removed %d stale uploads", n)
	}
}
