package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zelenin/go-tdlib/client"

	"github.com/FINWAX/tg-control-api-server/internal/tdjson"
)

// ErrBadFileRef is returned when the {file_id} path segment isn't an integer and
// no ?remote_id= was supplied.
var ErrBadFileRef = errors.New("file_id must be an integer, or pass ?remote_id=")

// DownloadFile ensures a file is present on this worker's disk and returns its
// local path plus the resolved local file id. It accepts either a numeric local
// file id (ref) or a persistent remote id — the latter is resolved to a local
// file via getRemoteFile first, so a crawler can fetch bytes later from stored
// references without keeping a live file_id. The download is synchronous: it
// blocks until TDLib has the whole file locally.
func (m *Manager) DownloadFile(ctx context.Context, id, ref, remoteID string) (path string, fileID int32, err error) {
	ls := m.get(id)
	if ls == nil {
		return "", 0, errors.New("session not found")
	}
	cl := ls.readyClient()
	if cl == nil {
		return "", 0, fmt.Errorf("session is %s, not authorized", ls.status_())
	}

	var fid int32
	if remoteID != "" {
		// fileTypeNone works for most types, but some (notably Photo) require the
		// concrete type. TDLib's rejection names the type ("...of type Photo..."),
		// so on that failure we retry with the correct fileType — no caller input.
		res, e := getRemoteFile(ctx, cl, remoteID, "fileTypeNone")
		if e != nil {
			if ft := fileTypeFromError(e); ft != "" {
				res, e = getRemoteFile(ctx, cl, remoteID, ft)
			}
			if e != nil {
				return "", 0, e
			}
		}
		if fid, e = fileIDFromObj(res); e != nil {
			return "", 0, e
		}
	} else {
		n, e := strconv.Atoi(ref)
		if e != nil {
			return "", 0, ErrBadFileRef
		}
		fid = int32(n)
	}

	q, _ := json.Marshal(map[string]any{
		"file_id": fid, "priority": 1, "offset": 0, "limit": 0, "synchronous": true,
	})
	res, err := tdjson.Call(ctx, cl, "downloadFile", q)
	if err != nil {
		return "", 0, err
	}
	p, err := localPathFromFile(res)
	if err != nil {
		return "", 0, err
	}
	return p, fid, nil
}

// DeleteTdFile drops a downloaded file from TDLib's storage (best-effort) so a
// crawl doesn't accumulate on disk. Used by the download endpoint's ?delete=1.
func (m *Manager) DeleteTdFile(ctx context.Context, id string, fileID int32) {
	ls := m.get(id)
	if ls == nil {
		return
	}
	cl := ls.readyClient()
	if cl == nil {
		return
	}
	q, _ := json.Marshal(map[string]any{"file_id": fileID})
	_, _ = tdjson.Call(ctx, cl, "deleteFile", q)
}

// getRemoteFile resolves a persistent remote id to a live file object using the
// given td_api fileType constructor.
func getRemoteFile(ctx context.Context, cl *client.Client, remoteID, fileType string) (json.RawMessage, error) {
	q, _ := json.Marshal(map[string]any{
		"remote_file_id": remoteID,
		"file_type":      map[string]any{"@type": fileType},
	})
	return tdjson.Call(ctx, cl, "getRemoteFile", q)
}

// fileTypeFromError extracts a td_api fileType constructor from a getRemoteFile
// rejection like "Can't use file of type Photo as <invalid>" -> "fileTypePhoto".
// Returns "" if the error isn't of that shape.
func fileTypeFromError(err error) string {
	var te *tdjson.Error
	if !errors.As(err, &te) {
		return ""
	}
	const marker = "of type "
	i := strings.Index(te.Message, marker)
	if i < 0 {
		return ""
	}
	rest := te.Message[i+len(marker):]
	j := 0
	for j < len(rest) && ((rest[j] >= 'A' && rest[j] <= 'Z') || (rest[j] >= 'a' && rest[j] <= 'z')) {
		j++
	}
	if j == 0 {
		return ""
	}
	return "fileType" + rest[:j]
}

// fileIDFromObj reads the local id out of a td_api file object.
func fileIDFromObj(res json.RawMessage) (int32, error) {
	var f struct {
		ID int32 `json:"id"`
	}
	if json.Unmarshal(res, &f) != nil || f.ID == 0 {
		return 0, errors.New("could not resolve remote file id")
	}
	return f.ID, nil
}

// localPathFromFile reads the on-disk path from a completed td_api file object.
func localPathFromFile(res json.RawMessage) (string, error) {
	var f struct {
		Local struct {
			Path string `json:"path"`
			Done bool   `json:"is_downloading_completed"`
		} `json:"local"`
	}
	if json.Unmarshal(res, &f) != nil {
		return "", errors.New("bad file object")
	}
	if f.Local.Path == "" {
		return "", errors.New("download did not complete (no local path)")
	}
	return f.Local.Path, nil
}
