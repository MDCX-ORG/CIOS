// Package wal is a tiny append-only write-ahead log used by the
// gateway's NATS publisher to buffer telemetry when the broker is
// unreachable (spec-006 §3.1). It is stdlib-only: the on-disk format
// is a sequence of 4-byte big-endian length-prefixed frames, each of
// which is the gzip-compressed bytes of the caller's payload
// (PRMT-016-Fix2, LOCKED L65).
//
// The format is intentionally simple so a half-written frame at the
// tail (process crash mid-Write) is detected and silently discarded
// on the next Open: a length that overruns the file is not a
// complete frame and we just truncate to the last good boundary.
//
// Concurrency: Write is guarded by an internal mutex; Replay takes
// the same lock. The caller must not hold either while invoking the
// replay callback (the callback runs without the lock).
package wal

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// ErrWALFull is returned by Write when appending the next frame
// would push the file past the per-instance cap (see OpenWithMaxSize
// and the package default MaxSize). The frame is not written.
// Callers are expected to drain via Replay (which truncates) and
// then retry.
var ErrWALFull = errors.New("wal: max size reached")

// MaxSize is the package-level DEFAULT cap on the WAL file. It is
// a var (not const) so tests can shrink it before calling Open,
// and so the per-instance OpenWithMaxSize override can fall back
// to it. Each WAL snapshots MaxSize (or an explicit override) into
// its own maxSize field at construction; the package variable is
// not consulted after Open.
var MaxSize int64 = 1 << 30 // 1 GiB default

// fileLike is the minimal file surface the WAL needs. *os.File
// satisfies it; tests inject a fake that fails Write at a chosen
// byte to exercise the rollback path. This is a test seam, NOT a
// public abstraction — fileLike and the field stay unexported.
type fileLike interface {
	io.Reader
	io.Writer
	io.Seeker
	Truncate(size int64) error
	Sync() error
	Close() error
}

// WAL is an append-only log. Construct with Open (default cap) or
// OpenWithMaxSize (explicit cap).
type WAL struct {
	path string
	mu   sync.Mutex
	f    fileLike // assigned a *os.File by Open
	// cachedLen is updated on every successful Write; Len() reads it
	// without re-scanning the file.
	cachedLen int
	// maxSize is the per-instance byte cap, snapshotted from the
	// package default MaxSize or the OpenWithMaxSize argument. All
	// size enforcement (ErrWALFull, recoverTail plausibility, Replay
	// upper bound) reads this field, not the package variable.
	maxSize int64
}

// Open opens (or creates) the WAL at path with the package default
// MaxSize cap. A partial tail frame — one whose declared length
// would overrun the file — is silently truncated; we seek back to
// the last fully-written frame and keep going.
func Open(path string) (*WAL, error) {
	return OpenWithMaxSize(path, MaxSize)
}

// OpenWithMaxSize opens the WAL at path with an explicit byte cap.
// maxBytes <= 0 falls back to the package default MaxSize. All
// per-WAL size checks use the resulting instance field; the
// package variable is not read again after this call.
func OpenWithMaxSize(path string, maxBytes int64) (*WAL, error) {
	if path == "" {
		return nil, errors.New("wal: path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("wal: mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("wal: open %s: %w", path, err)
	}
	cap := maxBytes
	if cap <= 0 {
		cap = MaxSize
	}
	w := &WAL{path: path, f: f, maxSize: cap}
	if err := w.recoverTail(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return w, nil
}

// recoverTail scans to the last complete frame, then truncates any
// partial bytes that follow it. A complete frame is 4 (length) + N
// (payload); an incomplete frame leaves the file shorter than the
// declared length, which we treat as garbage from a previous crash.
func (w *WAL) recoverTail() error {
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("wal: seek: %w", err)
	}
	var hdr [4]byte
	var off int64
	w.cachedLen = 0
	for {
		_, err := io.ReadFull(w.f, hdr[:])
		if err == io.EOF {
			return nil
		}
		if err == io.ErrUnexpectedEOF {
			// We read fewer than 4 header bytes at the tail. Truncate
			// the partial header and we're done.
			return w.truncateTo(off)
		}
		if err != nil {
			return fmt.Errorf("wal: read header: %w", err)
		}
		n := int64(binary.BigEndian.Uint32(hdr[:]))
		if n < 0 || n > w.maxSize {
			// Implausible length — treat the rest as garbage.
			return w.truncateTo(off)
		}
		// Need n more bytes. If they're not all there, the frame is
		// partial: truncate the whole frame (header + whatever payload
		// made it to disk) and stop.
		pos, _ := w.f.Seek(0, io.SeekCurrent)
		if pos+n > w.size() {
			return w.truncateTo(off)
		}
		if _, err := w.f.Seek(n, io.SeekCurrent); err != nil {
			return fmt.Errorf("wal: skip payload: %w", err)
		}
		off = off + 4 + n
		w.cachedLen++
	}
}

// Size returns the current on-disk WAL byte length (DATA-RESILIENCE G5 gauge).
func (w *WAL) Size() (int64, error) {
	if w == nil {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size(), nil
}

// size returns the current file size via Seek so the WAL depends only
// on io.ReadWriteSeeker + Truncate/Sync/Close (see fileLike below),
// which lets tests inject a write-failing fake.
func (w *WAL) size() int64 {
	cur, err := w.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0
	}
	end, err := w.f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0
	}
	_, _ = w.f.Seek(cur, io.SeekStart)
	return end
}

// truncateTo shrinks the file to sz bytes. It does NOT reset
// cachedLen; that count was already accumulated by recoverTail up
// to the boundary at sz (the only call site). Used only by
// recoverTail after a partial tail frame is detected.
func (w *WAL) truncateTo(sz int64) error {
	if err := w.f.Truncate(sz); err != nil {
		return fmt.Errorf("wal: truncate: %w", err)
	}
	if _, err := w.f.Seek(sz, io.SeekStart); err != nil {
		return fmt.Errorf("wal: seek post-truncate: %w", err)
	}
	// recoverTail has already accumulated cachedLen for every
	// complete frame up to sz; we do not touch it here.
	return nil
}

// Write appends one length-prefixed frame to the WAL and fsyncs
// the file before returning. A concurrent caller is serialised on
// the internal mutex. The payload is gzip-compressed before the
// length is computed and the bytes are written; the frame on disk
// is therefore [4 bytes BE length][gzip(frame)]. The frame is
// rejected with ErrWALFull when the COMPRESSED size would push the
// file past the per-instance cap (w.maxSize).
//
// If any of header-write, payload-write, or Sync fails, the file
// is rolled back to its pre-Write tail (Truncate + Seek) so no
// partial frame is left at the end of the WAL. cachedLen is only
// incremented on a fully-successful Write.
func (w *WAL) Write(frame []byte) error {
	// Compress outside the lock: gzip is pure-CPU and the lock is
	// only there to serialise file offset mutations. This keeps
	// concurrent Writes from blocking each other on compression.
	comp, err := gzipFrame(frame)
	if err != nil {
		return fmt.Errorf("wal: gzip: %w", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	need := int64(4 + len(comp))
	if w.size()+need > w.maxSize {
		return ErrWALFull
	}
	start, err := w.f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("wal: seek end: %w", err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(comp)))
	if _, err := w.f.Write(hdr[:]); err != nil {
		w.rollback(start)
		return fmt.Errorf("wal: write header: %w", err)
	}
	// gzip never returns zero bytes for non-nil input (it emits at
	// least the gzip header + trailer), so we always write comp.
	if _, err := w.f.Write(comp); err != nil {
		w.rollback(start)
		return fmt.Errorf("wal: write payload: %w", err)
	}
	if err := w.f.Sync(); err != nil {
		w.rollback(start)
		return fmt.Errorf("wal: sync: %w", err)
	}
	w.cachedLen++
	return nil
}

// rollback truncates the file back to off after a failed Write so no
// partial frame is left at the tail. Best-effort: a rollback failure
// is logged but cannot itself be recovered from here.
func (w *WAL) rollback(off int64) {
	if err := w.f.Truncate(off); err != nil {
		log.Printf("wal: rollback truncate to %d: %v", off, err)
		return
	}
	if _, err := w.f.Seek(off, io.SeekStart); err != nil {
		log.Printf("wal: rollback seek to %d: %v", off, err)
	}
}

// Len returns the number of complete frames in the WAL. The count
// is cached and updated on every successful Write / recover; a
// replay that truncates resets it to zero.
func (w *WAL) Len() (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cachedLen, nil
}

// Replay walks every stored frame in order, invoking fn for each.
// If fn returns a non-nil error, Replay stops immediately and
// returns that error without truncating. If every frame was
// delivered without error, the file is truncated to zero bytes
// (the WAL is not deleted; the file handle is preserved).
func (w *WAL) Replay(fn func(frame []byte) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("wal: seek: %w", err)
	}
	var hdr [4]byte
	for {
		_, err := io.ReadFull(w.f, hdr[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("wal: read header: %w", err)
		}
		n := int64(binary.BigEndian.Uint32(hdr[:]))
		if n > w.maxSize {
			return fmt.Errorf("wal: replay: frame length %d exceeds MaxSize %d", n, w.maxSize)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(w.f, buf); err != nil {
			return fmt.Errorf("wal: read payload: %w", err)
		}
		// Decompress before handing the frame to the caller. The
		// replay callback therefore sees the original payload, not
		// the on-disk gzip bytes — natspub.Publisher is unaffected.
		raw, derr := gunzipFrame(buf)
		if derr != nil {
			return fmt.Errorf("wal: gunzip: %w", derr)
		}
		if err := fn(raw); err != nil {
			return err
		}
	}

	if err := w.f.Truncate(0); err != nil {
		return fmt.Errorf("wal: truncate: %w", err)
	}
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("wal: seek post-truncate: %w", err)
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("wal: sync post-truncate: %w", err)
	}
	w.cachedLen = 0
	return nil
}

// Close closes the underlying file. It is a programming error to
// call any other method after Close; the mutex is still held by
// the calling code path so the file handle is not reused.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// --- gzip helpers --------------------------------------------------------

// gzipFrame compresses raw with stdlib gzip at the default
// compression level. The returned slice is the on-disk payload;
// the WAL then prepends a 4-byte big-endian length.
func gzipFrame(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gunzipFrame reverses gzipFrame. Any decode error (corrupt frame,
// truncated gzip stream) is surfaced to the caller; Replay wraps it
// with "wal: gunzip:" so the bad frame can be identified.
func gunzipFrame(comp []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(comp))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}
