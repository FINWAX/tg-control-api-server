package upload

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir(), 64<<20, 16<<20, 2<<30)
}

func TestValidateName(t *testing.T) {
	ok := []string{"pic.jpg", "Отчёт Q3.pdf", "a", strings.Repeat("x", 255)}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	bad := []string{"", ".", "..", metaName, "a/b", "a\\b", "a\x00b",
		"../escape", strings.Repeat("x", 256), strings.Repeat("ё", 200)} // 200 * 2 bytes > 255
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", n)
		}
	}
}

func TestSingleShot(t *testing.T) {
	s := newStore(t)
	data := []byte("hello world")
	p, size, err := s.SingleShot("greeting.txt", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("SingleShot: %v", err)
	}
	if size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", size, len(data))
	}
	if filepath.Base(p) != "greeting.txt" {
		t.Fatalf("path %q must keep the real name", p)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, data) {
		t.Fatalf("stored bytes = %q", got)
	}
	if !Within(p, s.dir) {
		t.Fatalf("stored path %q must be within %q", p, s.dir)
	}
}

func TestResumableRoundTrip(t *testing.T) {
	s := newStore(t)
	full := bytes.Repeat([]byte("ABCD"), 10) // 40 bytes
	id, err := s.Create("big.bin", int64(len(full)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// chunk 1
	off, err := s.Append(id, 0, bytes.NewReader(full[:16]))
	if err != nil || off != 16 {
		t.Fatalf("Append#1 = (%d, %v)", off, err)
	}
	// wrong offset is rejected
	if _, err := s.Append(id, 0, bytes.NewReader(full[16:])); err != ErrBadOffset {
		t.Fatalf("Append wrong offset err = %v, want ErrBadOffset", err)
	}
	// resume from reported offset
	cur, length, err := s.Status(id)
	if err != nil || cur != 16 || length != 40 {
		t.Fatalf("Status = (%d,%d,%v)", cur, length, err)
	}
	if off, err = s.Append(id, 16, bytes.NewReader(full[16:])); err != nil || off != 40 {
		t.Fatalf("Append#2 = (%d, %v)", off, err)
	}

	p, size, err := s.Complete(id)
	if err != nil || size != 40 {
		t.Fatalf("Complete = (%q,%d,%v)", p, size, err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, full) {
		t.Fatalf("assembled bytes mismatch: %q", got)
	}
}

func TestCompleteIncomplete(t *testing.T) {
	s := newStore(t)
	id, _ := s.Create("x.bin", 100)
	_, _ = s.Append(id, 0, bytes.NewReader(make([]byte, 40)))
	if _, _, err := s.Complete(id); err != ErrIncomplete {
		t.Fatalf("Complete on partial err = %v, want ErrIncomplete", err)
	}
}

func TestCreateTooLarge(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create("x.bin", s.maxTotal+1); err != ErrTooLarge {
		t.Fatalf("Create oversize err = %v, want ErrTooLarge", err)
	}
}

func TestBadIDAndNotFound(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Status("../etc"); err != ErrBadID {
		t.Fatalf("Status bad id err = %v, want ErrBadID", err)
	}
	if _, _, err := s.Status("00000000000000000000000000000000"); err != ErrNotFound {
		t.Fatalf("Status unknown id err = %v, want ErrNotFound", err)
	}
}

func TestSweepSparesActiveUpload(t *testing.T) {
	s := newStore(t)
	id, _ := s.Create("slow.bin", 100)
	dir := filepath.Join(s.dir, id)
	// The directory was created long ago (a multi-hour upload)...
	past := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(dir, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	// ...but a chunk just arrived: activity must refresh the TTL clock.
	if _, err := s.Append(id, 0, bytes.NewReader(make([]byte, 10))); err != nil {
		t.Fatalf("Append: %v", err)
	}
	n, err := s.Sweep(time.Now().Add(-2 * time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("Sweep removed an actively-appended upload: n=%d err=%v", n, err)
	}
	if _, _, err := s.Status(id); err != nil {
		t.Fatalf("active upload should survive, err = %v", err)
	}
}

func TestWithin(t *testing.T) {
	root := "/uploads"
	in := []string{"/uploads/ab/pic.jpg", "/uploads/x/y"}
	out := []string{"/uploads", "/etc/passwd", "/uploads-evil/x", "/uploads/../etc/passwd"}
	for _, p := range in {
		if !Within(p, root) {
			t.Errorf("Within(%q) = false, want true", p)
		}
	}
	for _, p := range out {
		if Within(p, root) {
			t.Errorf("Within(%q) = true, want false", p)
		}
	}
}

func TestSweep(t *testing.T) {
	s := newStore(t)
	id, _ := s.Create("keep.bin", 10)
	old, _ := s.Create("old.bin", 10)
	// backdate the "old" upload
	oldDir := filepath.Join(s.dir, old)
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldDir, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	n, err := s.Sweep(time.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("Sweep = (%d, %v), want 1", n, err)
	}
	if _, _, err := s.Status(old); err != ErrNotFound {
		t.Fatalf("old upload should be gone, err = %v", err)
	}
	if _, _, err := s.Status(id); err != nil {
		t.Fatalf("recent upload should survive, err = %v", err)
	}
}
