// Package upload is the shared-volume file store behind POST /v1/files. The
// gateway writes uploaded bytes here (single-shot or resumable/chunked); the
// worker that owns a session reads them back through inputFileLocal from the
// same volume, so bytes cross the network once (client -> gateway) and are then
// read locally by TDLib — never relayed gateway -> worker.
//
// Layout: each upload gets its own directory keyed by a server-generated id, and
// the file keeps its real name inside it, so TDLib (and thus Telegram) sees the
// original filename while the id-directory guarantees uniqueness and isolates
// path traversal:
//
//	<dir>/<id>/<original-name>       the file
//	<dir>/<id>/.upload.json          resumable metadata (name, length)
package upload

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// maxNameBytes is the hard cap on a stored filename: NAME_MAX (255 bytes) on
// ext4/xfs/btrfs — a longer name fails at create. Telegram separately truncates
// document names to ~63 chars, which is its concern, not ours.
const maxNameBytes = 255

// metaName is the reserved per-upload metadata file; a client filename may not
// collide with it.
const metaName = ".upload.json"

var idRe = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Errors surfaced to the HTTP layer, mapped to statuses there.
var (
	ErrBadName    = errors.New("invalid file name")
	ErrBadID      = errors.New("invalid upload id")
	ErrNotFound   = errors.New("upload not found")
	ErrBadOffset  = errors.New("offset does not match current length")
	ErrTooLarge   = errors.New("upload exceeds allowed size")
	ErrIncomplete = errors.New("upload is not fully received")
)

// Store persists uploads under dir on a volume shared with the workers.
type Store struct {
	dir       string
	maxSingle int64 // single-shot body cap
	maxChunk  int64 // per-chunk (PATCH) cap
	maxTotal  int64 // total upload cap (== Telegram's file-size ceiling)
}

func New(dir string, maxSingle, maxChunk, maxTotal int64) *Store {
	return &Store{dir: dir, maxSingle: maxSingle, maxChunk: maxChunk, maxTotal: maxTotal}
}

func (s *Store) MaxSingle() int64 { return s.maxSingle }
func (s *Store) MaxChunk() int64  { return s.maxChunk }
func (s *Store) MaxTotal() int64  { return s.maxTotal }

// ValidateName enforces the filename rules: a bare basename, no path separators
// or traversal, not the reserved meta name, and within the filesystem's 255-byte
// component limit. Content validity (allowed characters, Telegram's own limits)
// is deliberately left to Telegram.
func ValidateName(name string) error {
	if name == "" || name == "." || name == ".." || name == metaName {
		return ErrBadName
	}
	if strings.ContainsAny(name, "/\\\x00") || path.Base(name) != name {
		return ErrBadName
	}
	if len(name) > maxNameBytes {
		return ErrBadName
	}
	return nil
}

type meta struct {
	Name   string `json:"name"`
	Length int64  `json:"length"`
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// itemDir returns the on-disk directory for id after validating the id can't
// escape the store root.
func (s *Store) itemDir(id string) (string, error) {
	if !idRe.MatchString(id) {
		return "", ErrBadID
	}
	return filepath.Join(s.dir, id), nil
}

// SingleShot streams r to a fresh <dir>/<id>/<name>, capped at maxSingle, and
// returns the absolute path for inputFileLocal. The caller should wrap r in an
// http.MaxBytesReader so an oversize body is refused rather than filling disk.
func (s *Store) SingleShot(name string, r io.Reader) (fpath string, size int64, err error) {
	if err = ValidateName(name); err != nil {
		return "", 0, err
	}
	id, err := newID()
	if err != nil {
		return "", 0, err
	}
	dir := filepath.Join(s.dir, id)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	fpath = filepath.Join(dir, name)
	f, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		os.RemoveAll(dir)
		return "", 0, err
	}
	size, err = io.Copy(f, r)
	cerr := f.Close()
	if err != nil {
		os.RemoveAll(dir) // don't leave a partial single-shot file
		return "", 0, err
	}
	if cerr != nil {
		os.RemoveAll(dir)
		return "", 0, cerr
	}
	return fpath, size, nil
}

// Create begins a resumable upload of the declared length and returns its id.
func (s *Store) Create(name string, length int64) (id string, err error) {
	if err = ValidateName(name); err != nil {
		return "", err
	}
	if length <= 0 || length > s.maxTotal {
		return "", ErrTooLarge
	}
	if id, err = newID(); err != nil {
		return "", err
	}
	dir := filepath.Join(s.dir, id)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	mb, _ := json.Marshal(meta{Name: name, Length: length})
	if err = os.WriteFile(filepath.Join(dir, metaName), mb, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	// Pre-create the (empty) data file so Append/Status see offset 0.
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	f.Close()
	return id, nil
}

func (s *Store) readMeta(id string) (string, meta, error) {
	dir, err := s.itemDir(id)
	if err != nil {
		return "", meta{}, err
	}
	mb, err := os.ReadFile(filepath.Join(dir, metaName))
	if err != nil {
		if os.IsNotExist(err) {
			return "", meta{}, ErrNotFound
		}
		return "", meta{}, err
	}
	var m meta
	if err := json.Unmarshal(mb, &m); err != nil {
		return "", meta{}, err
	}
	return dir, m, nil
}

// Append writes a chunk at offset (which must equal the current length) and
// returns the new length. Per-chunk size is capped by the HTTP layer, which
// wraps r in an http.MaxBytesReader.
func (s *Store) Append(id string, offset int64, r io.Reader) (int64, error) {
	dir, m, err := s.readMeta(id)
	if err != nil {
		return 0, err
	}
	fpath := filepath.Join(dir, m.Name)
	fi, err := os.Stat(fpath)
	if err != nil {
		return 0, ErrNotFound
	}
	if offset != fi.Size() {
		return 0, ErrBadOffset
	}
	chunk, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	if offset+int64(len(chunk)) > m.Length {
		return 0, ErrTooLarge
	}
	f, err := os.OpenFile(fpath, os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := f.WriteAt(chunk, offset)
	if err != nil {
		return 0, err
	}
	// Refresh the directory's mtime so the TTL sweep measures inactivity, not age
	// since creation — an upload still receiving chunks (a slow multi-hour resume)
	// must not be swept mid-flight, since Sweep keys on the directory mtime and a
	// file write alone does not bump it.
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
	return offset + int64(n), nil
}

// Status returns the bytes received so far and the declared total, for resume.
func (s *Store) Status(id string) (offset, length int64, err error) {
	dir, m, err := s.readMeta(id)
	if err != nil {
		return 0, 0, err
	}
	fi, err := os.Stat(filepath.Join(dir, m.Name))
	if err != nil {
		return 0, 0, ErrNotFound
	}
	return fi.Size(), m.Length, nil
}

// Complete verifies the full length was received and returns the file path.
func (s *Store) Complete(id string) (fpath string, size int64, err error) {
	dir, m, err := s.readMeta(id)
	if err != nil {
		return "", 0, err
	}
	fpath = filepath.Join(dir, m.Name)
	fi, err := os.Stat(fpath)
	if err != nil {
		return "", 0, ErrNotFound
	}
	if fi.Size() != m.Length {
		return "", 0, ErrIncomplete
	}
	return fpath, fi.Size(), nil
}

// Abort removes an upload directory (any state). Idempotent.
func (s *Store) Abort(id string) error {
	dir, err := s.itemDir(id)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// Within reports whether p resolves inside root — the guard for inputFileLocal
// paths, so a session /call can only reference files under the uploads volume.
func Within(p, root string) bool {
	rp := filepath.Clean(root)
	cp := filepath.Clean(p)
	if cp == rp {
		return false // the root itself is not a file
	}
	return strings.HasPrefix(cp, rp+string(filepath.Separator))
}

// Sweep removes upload directories last modified before cutoff — the safety net
// for uploads that were never sent (or whose post-send delete was missed after a
// restart). Returns how many were removed.
func (s *Store) Sweep(cutoff time.Time) (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() || !idRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.RemoveAll(filepath.Join(s.dir, e.Name())); err != nil {
				return n, fmt.Errorf("sweep %s: %w", e.Name(), err)
			}
			n++
		}
	}
	return n, nil
}
