package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// tempWAL returns a freshly-opened WAL in a per-test temp dir. The
// cleanup closes and removes it; tests that need an explicit Close
// can call t.Cleanup themselves.
func tempWAL(t *testing.T) *WAL {
	t.Helper()
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "test.wal"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func TestWriteAndLen(t *testing.T) {
	w := tempWAL(t)
	for i := 0; i < 5; i++ {
		if err := w.Write([]byte(fmt.Sprintf("frame-%d", i))); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	n, err := w.Len()
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 5 {
		t.Errorf("Len = %d, want 5", n)
	}
}

func TestReplaySucceedsAndTruncates(t *testing.T) {
	w := tempWAL(t)
	want := [][]byte{
		[]byte("alpha"),
		[]byte("bravo"),
		[]byte("charlie"),
	}
	for _, f := range want {
		if err := w.Write(f); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	var got [][]byte
	if err := w.Replay(func(f []byte) error {
		// copy because the slice is reused on the next call
		buf := make([]byte, len(f))
		copy(buf, f)
		got = append(got, buf)
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("frame %d: got %q, want %q", i, got[i], want[i])
		}
	}
	// Truncate must have happened.
	n, err := w.Len()
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 0 {
		t.Errorf("Len after replay = %d, want 0", n)
	}
	st, err := os.Stat(filepath.Join(filepath.Dir(mustPath(t, w)), "test.wal"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// File should exist and be empty.
	if st.Size() != 0 {
		t.Errorf("file size = %d, want 0", st.Size())
	}
}

// mustPath uses reflect-free access to the package-private `path`
// field via a public file-stat helper we add at the bottom of this
// file. Keeps tests in the same package so the unexported field is
// reachable without exporting it.
func mustPath(t *testing.T, w *WAL) string {
	t.Helper()
	return w.path
}

func TestReplayStopsOnErrorAndKeepsWAL(t *testing.T) {
	w := tempWAL(t)
	for i := 0; i < 4; i++ {
		if err := w.Write([]byte(fmt.Sprintf("f%d", i))); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	sentinel := errors.New("nope")
	calls := 0
	err := w.Replay(func(f []byte) error {
		calls++
		if calls == 2 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Replay err = %v, want %v", err, sentinel)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	// Failed replay must not truncate: all four frames must still
	// be readable.
	n, err := w.Len()
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 4 {
		t.Errorf("Len after failed replay = %d, want 4", n)
	}
}

func TestPartialTailRecoveredOnOpen(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.wal")
	w, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Write two complete frames.
	if err := w.Write([]byte("ok-1")); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if err := w.Write([]byte("ok-2")); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	// Simulate a crash mid-Write: append 7 bytes that look like the
	// start of a length header (4 bytes = 0x00 0x00 0x00 0x05, i.e.
	// declaring a 5-byte payload) plus 3 bytes of payload.
	_ = w.Close()
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := f.Write([]byte{0x00, 0x00, 0x00, 0x05, 'a', 'b', 'c'}); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()

	// Reopen — recoverTail should discard the partial frame.
	w2, err := Open(p)
	if err != nil {
		t.Fatalf("Open after crash: %v", err)
	}
	defer w2.Close()
	n, err := w2.Len()
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 2 {
		t.Errorf("Len = %d, want 2 (partial tail must be dropped)", n)
	}
	var got []string
	if err := w2.Replay(func(f []byte) error {
		got = append(got, string(f))
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	want := []string{"ok-1", "ok-2"}
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestErrWALFull(t *testing.T) {
	// PRMT-016-Fix2: each frame is now gzip-compressed before the
	// length is computed. Two 50-byte runs of the same byte compress
	// to a small handful of bytes each, so the on-disk cap is the
	// COMPRESSED size. Use OpenWithMaxSize to pick a tight cap the
	// two compressed frames together will tip over, regardless of
	// how good gzip is at compressing 'a'x50.
	dir := t.TempDir()
	w, err := OpenWithMaxSize(filepath.Join(dir, "test.wal"), 32)
	if err != nil {
		t.Fatalf("OpenWithMaxSize: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	if err := w.Write(bytes.Repeat([]byte{'a'}, 50)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	err = w.Write(bytes.Repeat([]byte{'b'}, 50))
	if !errors.Is(err, ErrWALFull) {
		t.Fatalf("second write err = %v, want ErrWALFull", err)
	}
	// First frame still intact; second was rejected.
	var got [][]byte
	if err := w.Replay(func(f []byte) error {
		buf := make([]byte, len(f))
		copy(buf, f)
		got = append(got, buf)
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 1 || !bytes.Equal(got[0], bytes.Repeat([]byte{'a'}, 50)) {
		t.Errorf("got %v, want one frame of 'a'x50", got)
	}
}

func TestConcurrentWrites(t *testing.T) {
	w := tempWAL(t)
	const goroutines, perG = 8, 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				payload := fmt.Sprintf("g%d-i%d", g, i)
				if err := w.Write([]byte(payload)); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	n, err := w.Len()
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != goroutines*perG {
		t.Errorf("Len = %d, want %d", n, goroutines*perG)
	}
}

func TestOpenCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "deeply", "nested", "y.wal")
	w, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()
	if _, err := os.Stat(filepath.Dir(p)); err != nil {
		t.Errorf("parent dir not created: %v", err)
	}
}

// failWriter wraps a fileLike and fails the Nth byte of Write
// with the given error. It exists solely to drive the Write
// rollback path in wal_test.go. The wrapper is minimal: every
// other method is a direct passthrough.
type failWriter struct {
	f       fileLike
	written int64
	failAt  int64 // absolute byte offset (cumulative over Write calls) at which to fail
	failErr error
}

func (fw *failWriter) Read(p []byte) (int, error) { return fw.f.Read(p) }
func (fw *failWriter) Write(p []byte) (int, error) {
	if fw.failErr != nil {
		start := fw.written
		end := start + int64(len(p))
		// If the fail point is inside this call, write what we can
		// up to the fail boundary, then return the error. We do not
		// forward the remaining bytes — that's exactly the failure
		// the real OS would surface.
		if start < fw.failAt && fw.failAt < end {
			n := int(fw.failAt - start)
			w, _ := fw.f.Write(p[:n])
			fw.written += int64(w)
			return w, fw.failErr
		}
		if start >= fw.failAt {
			// We've already crossed the fail boundary on a prior
			// call; keep failing deterministically.
			return 0, fw.failErr
		}
	}
	n, err := fw.f.Write(p)
	fw.written += int64(n)
	return n, err
}
func (fw *failWriter) Seek(off int64, whence int) (int64, error) {
	return fw.f.Seek(off, whence)
}
func (fw *failWriter) Truncate(size int64) error { return fw.f.Truncate(size) }
func (fw *failWriter) Sync() error               { return fw.f.Sync() }
func (fw *failWriter) Close() error              { return fw.f.Close() }

// TestWriteRollbackOnPayloadFailure (PRMT-015b §4.2): if a Write
// fails midway through the payload, the file is rolled back to its
// pre-Write tail and cachedLen is not bumped. Subsequent normal
// Writes must still produce the expected frame sequence on Replay.
func TestWriteRollbackOnPayloadFailure(t *testing.T) {
	w := tempWAL(t)

	// Step 1: write a known-good frame so the file has some content.
	if err := w.Write([]byte("ok-1")); err != nil {
		t.Fatalf("Write ok-1: %v", err)
	}
	n, err := w.Len()
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 1 {
		t.Fatalf("Len after ok-1 = %d, want 1", n)
	}
	pre, err := w.f.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatalf("seek end: %v", err)
	}
	if pre == 0 {
		t.Fatalf("pre-Write end is 0; expected some bytes from ok-1")
	}

	// Step 2: inject a wrapper that fails the Write once it has
	// accepted at least 1 byte of the new payload. The Write call
	// will: succeed on the 4-byte header, then take 1 byte of the
	// payload, then fail — which is the precise failure mode the
	// rollback path is meant to handle.
	fw := &failWriter{f: w.f, failAt: pre + 4 + 1, failErr: errors.New("disk full")}
	w.f = fw

	werr := w.Write([]byte("this-frame-must-not-stick"))
	if werr == nil {
		t.Fatal("Write should have returned the injected error")
	}
	if !errors.Is(werr, fw.failErr) {
		t.Errorf("Write err = %v, want it to wrap the injected failure", werr)
	}

	// Step 3: file must have been rolled back to `pre`. Unwrap the
	// failWriter and stat the underlying file directly via the path
	// field to confirm the tail is clean.
	post, err := w.f.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatalf("seek end post: %v", err)
	}
	if post != pre {
		t.Errorf("post-Write file end = %d, want %d (rolled back)", post, pre)
	}
	// cachedLen must NOT have been bumped.
	n, err = w.Len()
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 1 {
		t.Errorf("Len after failed Write = %d, want 1 (cachedLen must not advance)", n)
	}

	// Step 4: replace with the real file again and write more
	// frames. Replay must yield ok-1 + the new frames in order with
	// no orphan bytes.
	w.f = fw.f
	if err := w.Write([]byte("ok-2")); err != nil {
		t.Fatalf("Write ok-2: %v", err)
	}
	if err := w.Write([]byte("ok-3")); err != nil {
		t.Fatalf("Write ok-3: %v", err)
	}
	var got []string
	if err := w.Replay(func(f []byte) error {
		got = append(got, string(f))
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	want := []string{"ok-1", "ok-2", "ok-3"}
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestReplayRejectsOversizeFrame (PRMT-015b §4.5): a frame whose
// declared length is > MaxSize at the moment of Replay must make
// Replay return an error rather than allocate a huge buffer.
//
// We shrink MaxSize for the duration of the test and append a
// bogus 4-byte length header (value 1024) directly via w.f.Write
// — bypassing the regular Write path so recoverTail (which also
// enforces n > MaxSize) does not see the bogus header at Open
// time. Then Replay is called with MaxSize=64, the bogus header
// exceeds it, and the call must error without calling fn for the
// bogus frame.
func TestReplayRejectsOversizeFrame(t *testing.T) {
	oldMax := MaxSize
	MaxSize = 64
	defer func() { MaxSize = oldMax }()

	w := tempWAL(t)
	// 1) Write one good frame the normal way. It uses 4 (header) +
	// len("alpha")=5 payload = 9 bytes, comfortably under MaxSize=64.
	if err := w.Write([]byte("alpha")); err != nil {
		t.Fatalf("Write good: %v", err)
	}
	// 2) Append a bogus header directly to the file, declaring a
	// 1024-byte payload — well under the default MaxSize (1 GiB) so
	// recoverTail would have accepted it, but > the shrunken
	// MaxSize=64. We write raw bytes, NOT through w.Write, to
	// bypass the ErrWALFull precheck.
	var bogus [4]byte
	binary.BigEndian.PutUint32(bogus[:], 1024)
	if _, err := w.f.Write(bogus[:]); err != nil {
		t.Fatalf("append bogus header: %v", err)
	}

	calls := 0
	rerr := w.Replay(func(f []byte) error {
		calls++
		return nil
	})
	if rerr == nil {
		t.Fatal("Replay should have returned an error for oversize frame")
	}
	// The good frame is delivered first; the bogus one bails before
	// fn is invoked for it.
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (good frame before oversize error)", calls)
	}
}

// TestCompressionRoundTrip (PRMT-016-Fix2 §2-bis.1 / §5): Write
// takes plaintext, the on-disk format is gzip-compressed, but the
// Replay callback receives the original plaintext byte-for-byte.
// This is the transparency contract for natspub.Publisher.
func TestCompressionRoundTrip(t *testing.T) {
	w := tempWAL(t)
	inputs := [][]byte{
		[]byte("alpha"),
		bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 20),
		[]byte(""),
		bytes.Repeat([]byte("\x00\x01\x02\x03"), 64),
		// Realistic Prometheus text: a couple of metrics with
		// repeated label keys → gzip should crush this.
		[]byte(`metric_a{label="x"} 1.5 1700000000000` + "\n" +
			`metric_b{label="x"} 2.5 1700000000001` + "\n" +
			`metric_c{label="x"} 3.5 1700000000002` + "\n"),
	}
	for i, in := range inputs {
		// Empty []byte is a valid frame (gzip("") is a few bytes of
		// header + trailer, not zero bytes). Make sure both the
		// happy path and the empty case round-trip.
		cp := make([]byte, len(in))
		copy(cp, in)
		if err := w.Write(cp); err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
	}
	var got [][]byte
	if err := w.Replay(func(f []byte) error {
		buf := make([]byte, len(f))
		copy(buf, f)
		got = append(got, buf)
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != len(inputs) {
		t.Fatalf("len = %d, want %d", len(got), len(inputs))
	}
	for i := range inputs {
		if !bytes.Equal(got[i], inputs[i]) {
			t.Errorf("frame %d: got %q, want %q (round-trip mismatch)", i, got[i], inputs[i])
		}
	}
}

// TestCompressionActuallyCompresses (PRMT-016-Fix2 §2-bis.2 / §5):
// on-disk bytes must be measurably smaller than the plaintext sum
// for compressible input. We use a run of 'x' (highly redundant)
// long enough that the per-frame gzip header overhead is
// negligible.
func TestCompressionActuallyCompresses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	// 5 frames, each 4 KiB of 'x'. Plaintext total = 20 KiB.
	const frames, frameLen = 5, 4096
	payload := bytes.Repeat([]byte{'x'}, frameLen)
	plaintextTotal := frames * frameLen
	for i := 0; i < frames; i++ {
		if err := w.Write(payload); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// We require the on-disk file to be at most 25% of the
	// plaintext total — a deliberately generous bound that
	// accommodates the per-frame gzip header + 4-byte length
	// prefix. In practice the ratio is ~1%.
	maxDiskBytes := int64(plaintextTotal) / 4
	if st.Size() >= maxDiskBytes {
		t.Errorf("on-disk size = %d, want < %d (gzip should crush %d repeated bytes)",
			st.Size(), maxDiskBytes, plaintextTotal)
	}
	t.Logf("plaintext %d B → on-disk %d B (ratio %.2f%%)",
		plaintextTotal, st.Size(), 100*float64(st.Size())/float64(plaintextTotal))
}

// TestOpenWithMaxSizeOverridesDefault (PRMT-016-Fix2 §4.1 / §5):
// the per-instance cap from OpenWithMaxSize is enforced
// independently of the package default MaxSize, and a <=0 arg
// falls back to the default.
func TestOpenWithMaxSizeOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	// Snap the default up to something huge so we can be sure the
	// WAL's per-instance cap is what's tripping ErrWALFull.
	oldMax := MaxSize
	MaxSize = 1 << 30
	defer func() { MaxSize = oldMax }()

	// Tiny cap that the first write will already exceed. 64 KiB
	// of 'q' compresses well below 64 B, so the per-instance cap
	// trips at Write time.
	big := bytes.Repeat([]byte{'q'}, 64*1024)
	wSmall, err := OpenWithMaxSize(filepath.Join(dir, "small.wal"), 64)
	if err != nil {
		t.Fatalf("OpenWithMaxSize small: %v", err)
	}
	defer wSmall.Close()
	if werr := wSmall.Write(big); !errors.Is(werr, ErrWALFull) {
		t.Errorf("small WAL Write: err = %v, want ErrWALFull (cap=64)", werr)
	}

	// <=0 falls back to the (huge) default — same payload fits.
	wFallback, err := OpenWithMaxSize(filepath.Join(dir, "fallback.wal"), 0)
	if err != nil {
		t.Fatalf("OpenWithMaxSize fallback: %v", err)
	}
	defer wFallback.Close()
	if werr := wFallback.Write(big); werr != nil {
		t.Errorf("fallback WAL Write: err = %v, want nil (cap=MaxSize)", werr)
	}
	n, err := wFallback.Len()
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 1 {
		t.Errorf("fallback Len = %d, want 1", n)
	}

	// Two WALs opened with different caps on the same file path
	// must each enforce their own cap — opening the small WAL on
	// top of the fallback WAL is also a good "second instance on
	// the same file" sanity check (the file is reopened RW; the
	// per-instance maxSize is what matters for the next Write).
	wSmall2, err := OpenWithMaxSize(filepath.Join(dir, "fallback.wal"), 64)
	if err != nil {
		t.Fatalf("OpenWithMaxSize small2: %v", err)
	}
	defer wSmall2.Close()
	if werr := wSmall2.Write(big); !errors.Is(werr, ErrWALFull) {
		t.Errorf("small2 WAL Write on top of fallback: err = %v, want ErrWALFull (cap=64)", werr)
	}
}
