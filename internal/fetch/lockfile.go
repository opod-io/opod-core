package fetch

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// A download lock is a FILE whose existence means "held" — that is the
// protocol every opod since the first cache understands, so it stays. What a
// file cannot say is whether its holder is still alive: a puller that was
// SIGKILLed, OOM-killed or lost with its node leaves the file behind, and a
// waiter could only guess from its age (LockWait, six hours, because a live
// 40 GB pull legitimately takes hours).
//
// So a holder ALSO takes flock(2) on the file and keeps the descriptor open
// for as long as it holds the lock. The kernel drops that flock the moment the
// process dies, however it dies, so a waiter that can take it knows the holder
// is gone — exactly, not by age. The holder writes flockMarker into the file
// AFTER it has the flock; a lock without the marker was written by an older
// opod that takes no flock, and only the age rule applies to it. That is what
// keeps a mixed fleet safe while nodes upgrade: a new binary never mistakes an
// old binary's live pull for a dead one.
//
// flock is per open file description on Linux and Darwin (the only release
// targets), so two descriptors in one process exclude each other too.
const flockMarker = "held-by-flock"

// lockFile is one held lock: the descriptor carries the flock.
type lockFile struct {
	path string
	f    *os.File
}

// acquireLockFile creates path exclusively and takes its flock. ok=false with
// a nil error means the file already exists — someone holds it, or held it.
func acquireLockFile(path, holder string) (lf *lockFile, ok bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// We created the file, so nobody else has its flock yet; a failure here is
	// a filesystem without flock, and the lock still works as an existence lock
	// — it just cannot be proven dead, so no marker is written.
	marker := ""
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		marker = " " + flockMarker
	}
	_, _ = fmt.Fprintf(f, "%s pid %d at %s%s\n", holder, os.Getpid(), time.Now().UTC().Format(time.RFC3339), marker)
	return &lockFile{path: path, f: f}, true, nil
}

// release removes the file BEFORE it drops the flock, so a waiter that sees
// the flock come free on this inode also sees the path gone or replaced.
func (l *lockFile) release() {
	_ = os.Remove(l.path)
	_ = l.f.Close()
}

// lockHolderGone reports whether the lock at path was taken by a holder that
// flocked it and is no longer alive. false for a live holder, a lock written by
// an opod that takes no flock (the age rule decides those), or a path that is
// gone or was replaced while we looked.
func lockHolderGone(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(b, []byte(flockMarker)) {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return false // the holder is alive and still has it
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	// We have the flock on the inode we opened. It is the holder's only if the
	// path still names that inode: a holder that released normally removed the
	// path first, and a new holder may already have created a fresh one.
	opened, err1 := f.Stat()
	now, err2 := os.Stat(path)
	return err1 == nil && err2 == nil && os.SameFile(opened, now)
}
