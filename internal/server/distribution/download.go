package distribution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// downloadRounds is how many times every source is tried for one file.
const downloadRounds = 2

// Progress reports a transfer: bytes on disk so far out of the file's size,
// and the source they are coming from.
type Progress struct {
	Source string
	Done   int64
	Total  int64
}

// errBadContent is a transfer that completed but does not match the index;
// the partial file is discarded rather than resumed.
var errBadContent = errors.New("content does not match the index")

// Download writes file to dest. The source that served the index is tried
// first, then the others (those that answered fastest first). A transfer that
// fails or stalls resumes from where it stopped on the next source; the
// finished file must match the index's size and sha256 before it is renamed
// into place. It returns the source the last bytes came from.
func (c *Client) Download(ctx context.Context, fetched *Fetched, file File, dest string, progress func(Progress)) (string, error) {
	if err := file.validate(); err != nil {
		return "", err
	}
	part := dest + ".part"
	var failures []string
	for round := 0; round < downloadRounds; round++ {
		for _, source := range fetched.fileOrder() {
			err := c.fetchInto(ctx, source, file, part, progress)
			if err == nil {
				err = verifyFile(part, file)
				if err == nil {
					if err := os.Rename(part, dest); err != nil {
						return "", err
					}
					return source, nil
				}
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if errors.Is(err, errBadContent) {
				_ = os.Remove(part)
			}
			failures = append(failures, fmt.Sprintf("%s: %v", source, err))
		}
	}
	return "", fmt.Errorf("download %s failed from every source: %s", file.Path, strings.Join(failures, "; "))
}

// ReadFile fetches a small file into memory with the same source order and
// checks as Download, without resuming.
func (c *Client) ReadFile(ctx context.Context, fetched *Fetched, file File) ([]byte, string, error) {
	if err := file.validate(); err != nil {
		return nil, "", err
	}
	var failures []string
	for round := 0; round < downloadRounds; round++ {
		for _, source := range fetched.fileOrder() {
			var buf bytes.Buffer
			err := c.transfer(ctx, source, file, 0, &buf, progressNone)
			if err == nil {
				err = checkContent(buf.Bytes(), file)
				if err == nil {
					return buf.Bytes(), source, nil
				}
			}
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			failures = append(failures, fmt.Sprintf("%s: %v", source, err))
		}
	}
	return nil, "", fmt.Errorf("download %s failed from every source: %s", file.Path, strings.Join(failures, "; "))
}

// fileOrder is the order sources are tried for files: the index's source,
// then the others that answered by speed, then those that did not answer.
func (f *Fetched) fileOrder() []string {
	order := []string{f.Source}
	var answered, rest []Attempt
	for _, attempt := range f.Attempts {
		switch {
		case attempt.Source == f.Source:
		case attempt.Err == nil:
			answered = append(answered, attempt)
		default:
			rest = append(rest, attempt)
		}
	}
	sort.SliceStable(answered, func(i, j int) bool { return answered[i].Latency < answered[j].Latency })
	for _, attempt := range append(answered, rest...) {
		order = append(order, attempt.Source)
	}
	return order
}

// fetchInto appends the rest of file to part, starting from what part holds.
func (c *Client) fetchInto(ctx context.Context, source string, file File, part string, progress func(Progress)) error {
	have := int64(0)
	if info, err := os.Stat(part); err == nil {
		have = info.Size()
	}
	if have > file.Size {
		have = 0
	}
	if have == file.Size {
		return nil
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if have == 0 {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	out, err := os.OpenFile(part, flags, 0o600)
	if err != nil {
		return err
	}
	err = c.transfer(ctx, source, file, have, out, progress)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}

// transfer GETs file from source starting at offset and writes it to w. It
// fails when nothing arrives for the stall timeout, and when the response is
// not the requested range: a source that answers a range request with the
// whole file fails with errBadContent, so the caller discards the partial
// copy and the next attempt starts from zero.
func (c *Client) transfer(ctx context.Context, source string, file File, offset int64, w io.Writer, progress func(Progress)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := c.stallTimeout()
	var stalled atomic.Bool
	timer := time.AfterFunc(stall, func() { stalled.Store(true); cancel() })
	defer timer.Stop()

	endpoint := source + "/" + file.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return stallError(stalled.Load(), stall, err)
	}
	defer resp.Body.Close()
	switch {
	case offset > 0 && resp.StatusCode == http.StatusPartialContent:
		if start, ok := contentRangeStart(resp.Header.Get("Content-Range")); !ok || start != offset {
			return fmt.Errorf("GET %s: range starts at %q, want %d", endpoint, resp.Header.Get("Content-Range"), offset)
		}
	case offset == 0 && resp.StatusCode == http.StatusOK:
	case offset > 0 && resp.StatusCode == http.StatusOK:
		// The source ignored the range. Its body is the whole file, which this
		// writer cannot take; the partial copy is discarded so the next try
		// starts clean.
		return fmt.Errorf("GET %s: range not honoured: %w", endpoint, errBadContent)
	case offset > 0 && resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		// The object is no longer than what is already on disk, so it is not
		// the file the index lists.
		return fmt.Errorf("GET %s: object ends before byte %d, the index lists %d bytes: %w", endpoint, offset, file.Size, errBadContent)
	default:
		return fmt.Errorf("GET %s: %s", endpoint, resp.Status)
	}
	want := file.Size - offset
	if resp.ContentLength >= 0 && resp.ContentLength != want {
		// A complete answer of the wrong length is another object (a mirror
		// halfway through a sync), not a cut connection worth resuming.
		return fmt.Errorf("GET %s: source sends %d bytes from byte %d, the index lists %d in all: %w", endpoint, resp.ContentLength, offset, file.Size, errBadContent)
	}
	done := offset
	buf := make([]byte, 256<<10)
	body := io.LimitReader(resp.Body, want+1)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			timer.Reset(stall)
			if done+int64(n) > file.Size {
				return fmt.Errorf("GET %s: more than the %d bytes the index lists: %w", endpoint, file.Size, errBadContent)
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
			done += int64(n)
			progress(Progress{Source: source, Done: done, Total: file.Size})
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return stallError(stalled.Load(), stall, fmt.Errorf("GET %s: %w", endpoint, readErr))
		}
	}
	if done != file.Size {
		return fmt.Errorf("GET %s: connection closed after %d of %d bytes", endpoint, done, file.Size)
	}
	return nil
}

func progressNone(Progress) {}

func stallError(stalled bool, stall time.Duration, err error) error {
	if stalled {
		return fmt.Errorf("no data for %s: %w", stall, err)
	}
	return err
}

// contentRangeStart parses the first byte of "bytes <start>-<end>/<size>".
func contentRangeStart(header string) (int64, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes ")
	if !ok {
		return 0, false
	}
	first, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	start, err := strconv.ParseInt(first, 10, 64)
	return start, err == nil
}

// verifyFile checks a finished file against the index.
func verifyFile(path string, file File) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hasher := sha256.New()
	n, err := io.Copy(hasher, f)
	if err != nil {
		return err
	}
	return checkDigest(n, hex.EncodeToString(hasher.Sum(nil)), file)
}

func checkContent(data []byte, file File) error {
	sum := sha256.Sum256(data)
	return checkDigest(int64(len(data)), hex.EncodeToString(sum[:]), file)
}

func checkDigest(size int64, digest string, file File) error {
	if size != file.Size {
		return fmt.Errorf("%s is %d bytes, the index lists %d: %w", file.Path, size, file.Size, errBadContent)
	}
	if digest != file.SHA256 {
		return fmt.Errorf("%s sha256 %s, the index lists %s: %w", file.Path, digest, file.SHA256, errBadContent)
	}
	return nil
}
