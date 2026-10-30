package batch

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"sync"
)

// A batch's results are appended to one partial file as requests finish, in
// the order they finish: a line of the OpenAI output format, whether it is a
// result or an error. Finishing splits it into the output and the error file,
// one line per custom_id (the last wins). The partial file is what a restart
// resumes from: its durable length is the batch's checkpoint offset.

// resultLine is a line of an output or error file (and of the partial file).
type resultLine struct {
	ID       string          `json:"id"`
	CustomID string          `json:"custom_id"`
	Response *resultResponse `json:"response"`
	Error    *resultError    `json:"error"`
}

type resultResponse struct {
	StatusCode int             `json:"status_code"`
	RequestID  string          `json:"request_id"`
	Body       json.RawMessage `json:"body"`
}

type resultError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// requestID is the id of a request's line in the output: the same for every
// attempt and every resume of the same request.
func requestID(batchID, customID string) string {
	sum := sha256.Sum256([]byte(batchID + "\x00" + customID))
	return "batch_req_" + hex.EncodeToString(sum[:12])
}

// encodeResult is a result line for a request that got a reply.
func encodeResult(batchID, customID string, reply Reply, fail *resultError) []byte {
	line := resultLine{ID: requestID(batchID, customID), CustomID: customID, Error: fail}
	if reply.Status != 0 {
		line.Response = &resultResponse{StatusCode: reply.Status, RequestID: reply.RequestID, Body: bodyJSON(reply.Body)}
	}
	return mustMarshal(line)
}

// encodeFailure is a result line for a request that got no reply.
func encodeFailure(batchID, customID, code, message string) []byte {
	return mustMarshal(resultLine{ID: requestID(batchID, customID), CustomID: customID, Error: &resultError{Code: code, Message: message}})
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Every field is a string, number or valid raw JSON.
		panic(fmt.Sprintf("batch: encode a result: %v", err))
	}
	return b
}

// bodyJSON is a response body as a JSON value: itself when it is valid JSON,
// else a JSON string of its text, so one odd reply cannot corrupt the file.
func bodyJSON(body []byte) json.RawMessage {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return json.RawMessage("null")
	}
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	return mustMarshal(string(body))
}

// customHash is the key a request is remembered by while a batch runs: 64
// bits keep a 50,000-request batch's set at a couple of megabytes.
func customHash(customID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(customID))
	return h.Sum64()
}

// partial is a batch's partial result file.
type partial struct {
	mu        sync.Mutex
	f         *os.File
	w         *bufio.Writer
	offset    int64
	completed int
	failed    int
	dirty     bool
	done      map[uint64]struct{}
}

// openPartial opens the batch's partial file for appending after its last
// durable byte. Whatever follows checkpoint (a torn tail, or lines a crash
// kept from the last checkpoint) is cut off, then the file is read back to
// know which requests have a result and how many succeeded. A file shorter
// than the checkpoint lost data: the batch starts over.
func openPartial(path string, checkpoint int64) (*partial, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		return nil, err
	}
	p := &partial{f: f, done: map[uint64]struct{}{}}
	fail := func(err error) (*partial, error) {
		_ = f.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if info.Size() < checkpoint {
		checkpoint = 0
	}
	if err := f.Truncate(checkpoint); err != nil {
		return fail(err)
	}
	rd := bufio.NewReaderSize(io.NewSectionReader(f, 0, checkpoint), 256<<10)
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			var res struct {
				CustomID string          `json:"custom_id"`
				Error    json.RawMessage `json:"error"`
			}
			if json.Unmarshal(line, &res) != nil {
				return fail(fmt.Errorf("the partial result file is corrupt at byte %d", p.offset))
			}
			if _, seen := p.done[customHash(res.CustomID)]; !seen {
				p.done[customHash(res.CustomID)] = struct{}{}
				if isNull(res.Error) {
					p.completed++
				} else {
					p.failed++
				}
			}
			p.offset += int64(len(line))
		}
		if err != nil {
			break
		}
	}
	if _, err := f.Seek(p.offset, io.SeekStart); err != nil {
		return fail(err)
	}
	p.w = bufio.NewWriterSize(f, 64<<10)
	return p, nil
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

// has reports whether the request already has a result.
func (p *partial) has(customID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.done[customHash(customID)]
	return ok
}

// append adds a result line (without its newline).
func (p *partial) append(customID string, line []byte, succeeded bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.w.Write(line); err != nil {
		return err
	}
	if err := p.w.WriteByte('\n'); err != nil {
		return err
	}
	p.offset += int64(len(line)) + 1
	p.done[customHash(customID)] = struct{}{}
	if succeeded {
		p.completed++
	} else {
		p.failed++
	}
	p.dirty = true
	return nil
}

// checkpoint makes what was appended durable and returns the figures that go
// with it; ok is false when nothing was appended since the last checkpoint.
func (p *partial) checkpoint() (prog Progress, ok bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.dirty {
		return Progress{Completed: p.completed, Failed: p.failed, Offset: p.offset}, false, nil
	}
	if err := p.w.Flush(); err != nil {
		return Progress{}, false, err
	}
	if err := p.f.Sync(); err != nil {
		return Progress{}, false, err
	}
	p.dirty = false
	return Progress{Completed: p.completed, Failed: p.failed, Offset: p.offset}, true, nil
}

func (p *partial) counts() (completed, failed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.completed, p.failed
}

func (p *partial) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	werr := p.w.Flush()
	if err := p.f.Close(); werr == nil {
		werr = err
	}
	return werr
}

// assembled is what splitting a partial file made.
type assembled struct {
	completed, failed  int
	outBytes, errBytes int64
}

// assemble writes the output and error files from a closed partial file: one
// line per custom_id, the last one written, a succeeded request to the output
// and a failed one to the error file. An empty file is not created.
func assemble(partialPath, outPath, errPath string) (assembled, error) {
	var res assembled
	last, err := lastLines(partialPath)
	if err != nil {
		return res, err
	}
	in, err := os.Open(partialPath)
	if err != nil {
		return res, err
	}
	defer in.Close()
	out, err := newLazyFile(outPath)
	if err != nil {
		return res, err
	}
	fails, err := newLazyFile(errPath)
	if err != nil {
		out.abort()
		return res, err
	}
	rd := bufio.NewReaderSize(in, 256<<10)
	var offset int64
	for {
		line, rerr := rd.ReadBytes('\n')
		if len(line) > 0 {
			start := offset
			offset += int64(len(line))
			var parsed struct {
				CustomID string          `json:"custom_id"`
				Error    json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(line, &parsed); err != nil {
				out.abort()
				fails.abort()
				return res, err
			}
			if last[parsed.CustomID] == start {
				dst := out
				if !isNull(parsed.Error) {
					dst = fails
				}
				if err := dst.write(line); err != nil {
					out.abort()
					fails.abort()
					return res, err
				}
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				out.abort()
				fails.abort()
				return res, rerr
			}
			break
		}
	}
	if res.outBytes, res.completed, err = out.commit(); err != nil {
		fails.abort()
		return res, err
	}
	if res.errBytes, res.failed, err = fails.commit(); err != nil {
		return res, err
	}
	return res, nil
}

// lastLines maps each custom_id of a partial file to the offset of its last line.
func lastLines(path string) (map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	last := map[string]int64{}
	rd := bufio.NewReaderSize(f, 256<<10)
	var offset int64
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			var parsed struct {
				CustomID string `json:"custom_id"`
			}
			if json.Unmarshal(line, &parsed) != nil {
				return nil, fmt.Errorf("the partial result file is corrupt at byte %d", offset)
			}
			last[parsed.CustomID] = offset
			offset += int64(len(line))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return last, nil
			}
			return nil, err
		}
	}
}

// lazyFile is a file written to a temporary name and put in place on commit,
// created only when its first line is written.
type lazyFile struct {
	path  string
	f     *os.File
	w     *bufio.Writer
	bytes int64
	lines int
}

func newLazyFile(path string) (*lazyFile, error) { return &lazyFile{path: path}, nil }

func (l *lazyFile) write(line []byte) error {
	if l.f == nil {
		f, err := os.OpenFile(l.path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
		if err != nil {
			return err
		}
		l.f = f
		l.w = bufio.NewWriterSize(f, 64<<10)
	}
	n, err := l.w.Write(line)
	l.bytes += int64(n)
	l.lines++
	return err
}

func (l *lazyFile) commit() (int64, int, error) {
	if l.f == nil {
		_ = os.Remove(l.path) // a rerun that has no such file now
		return 0, 0, nil
	}
	err := l.w.Flush()
	if err == nil {
		err = l.f.Sync()
	}
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(l.path+".tmp", l.path)
	}
	if err != nil {
		_ = os.Remove(l.path + ".tmp")
		return 0, 0, err
	}
	return l.bytes, l.lines, nil
}

func (l *lazyFile) abort() {
	if l.f != nil {
		_ = l.f.Close()
		_ = os.Remove(l.path + ".tmp")
	}
}
