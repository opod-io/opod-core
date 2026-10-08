package batch

// The progress record (see the package comment): sent.jsonl says which lines
// may have left this process, the two result files say which are finished.
// Everything a resume needs is read once, at open; afterwards the record only
// appends, under one lock, and fsyncs the sent mark before its request goes.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type record struct {
	mu        sync.Mutex
	sentF     *os.File
	outF      *os.File
	errF      *os.File
	outPath   string
	errPath   string
	cancelled bool

	// Read at open and never written afterwards, so the dispatcher may read
	// them without the lock: what the PREVIOUS runs did.
	sent      map[int]bool
	finished  map[int]bool
	okCount   int
	failCount int
}

func openRecord(dir string) (*record, error) {
	r := &record{
		sent: map[int]bool{}, finished: map[int]bool{},
		outPath: filepath.Join(dir, OutputFile), errPath: filepath.Join(dir, ErrorFile),
	}
	var err error
	sentPath := filepath.Join(dir, SentFile)
	if err = repairTail(sentPath); err != nil {
		return nil, err
	}
	if err = scan(sentPath, func(b []byte) {
		var m struct {
			Line *int `json:"line"`
		}
		if json.Unmarshal(b, &m) == nil && m.Line != nil {
			r.sent[*m.Line] = true
		}
	}); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		path string
		n    *int
	}{{r.outPath, &r.okCount}, {r.errPath, &r.failCount}} {
		if err = repairTail(f.path); err != nil {
			return nil, err
		}
		if err = scan(f.path, func(b []byte) {
			var o struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(b, &o) != nil {
				return
			}
			if n, ok := lineOf(o.ID); ok && !r.finished[n] {
				r.finished[n] = true
				*f.n++
			}
		}); err != nil {
			return nil, err
		}
	}
	if r.sentF, err = appendOnly(sentPath); err != nil {
		return nil, err
	}
	if r.outF, err = appendOnly(r.outPath); err != nil {
		r.close()
		return nil, err
	}
	if r.errF, err = appendOnly(r.errPath); err != nil {
		r.close()
		return nil, err
	}
	return r, nil
}

// markSent records that line n is about to be sent, durably, before it is.
func (r *record) markSent(n int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := fmt.Fprintf(r.sentF, "{\"line\":%d}\n", n); err != nil {
		return err
	}
	return r.sentF.Sync()
}

// result writes line n's answer to the output file (ok) or the error file.
// One write per line, so a kill leaves at most one partial line at the end of
// a file, which repairTail removes on the next open.
func (r *record) result(n int, o OutputLine, ok bool) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.errF
	if ok {
		f = r.outF
	}
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("batch: write the result of line %d: %w", n, err)
	}
	return f.Sync()
}

func (r *record) noteCancelled() {
	r.mu.Lock()
	r.cancelled = true
	r.mu.Unlock()
}

func (r *record) cancelledAny() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled
}

func (r *record) close() {
	for _, f := range []*os.File{r.sentF, r.outF, r.errF} {
		if f != nil {
			f.Close()
		}
	}
}

func appendOnly(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("batch: %w", err)
	}
	return f, nil
}

// scan calls fn with every complete line of path; a missing file has none.
func scan(path string, fn func([]byte)) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("batch: %w", err)
	}
	defer f.Close()
	rd := bufio.NewReaderSize(f, 1<<20)
	for {
		b, err := rd.ReadBytes('\n')
		if t := bytes.TrimSpace(b); len(t) > 0 {
			fn(t)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("batch: %w", err)
		}
	}
}

// repairTail cuts a file back to its last newline: a kill in the middle of a
// write leaves a partial last line, and a partial line read as finished (or
// appended to) would corrupt the record. A line cut here was never finished,
// which is exactly what the next open must conclude.
func repairTail(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("batch: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	// Walk back from the end in blocks to the last '\n'.
	const block = 64 << 10
	end := st.Size()
	buf := make([]byte, block)
	for pos := end; pos > 0; {
		n := int64(block)
		if pos < n {
			n = pos
		}
		pos -= n
		if _, err := f.ReadAt(buf[:n], pos); err != nil && err != io.EOF {
			return fmt.Errorf("batch: %w", err)
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			keep := pos + int64(i) + 1
			if keep == end {
				return nil
			}
			return f.Truncate(keep)
		}
	}
	return f.Truncate(0)
}
