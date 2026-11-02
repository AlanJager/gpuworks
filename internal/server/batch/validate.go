package batch

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

// inputLine is one request of an input file.
type inputLine struct {
	CustomID json.RawMessage `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

// inputStats is what validating an input file found.
type inputStats struct {
	Bytes int64
	Lines int
	// Endpoint is the url every request asks for, "" when they differ.
	Endpoint string
	// MixedLine is the first line whose url differs from the first request's,
	// 0 when they all ask for the same one.
	MixedLine int
}

// fileLimit is the size an input file may have; a test lowers it.
var fileLimit int64 = MaxFileBytes

var errFileTooLarge = errors.New("input file too large")

// limitReader reads up to max bytes of r and then fails with errFileTooLarge
// instead of ending, so a longer file is told apart from one that ends there.
type limitReader struct {
	r    io.Reader
	left int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		var one [1]byte
		if n, _ := l.r.Read(one[:]); n > 0 {
			return 0, errFileTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}

// scanInput validates a JSONL file of requests as it copies it to w: each
// non-blank line is a JSON object with a unique non-empty string custom_id,
// method POST, a url of the allowed endpoints and a body object naming a
// model and not asking for a stream. The error names the line.
func scanInput(src io.Reader, w io.Writer) (inputStats, *Error) {
	var st inputStats
	counted := &countingWriter{w: w}
	br := bufio.NewReaderSize(io.TeeReader(&limitReader{r: src, left: fileLimit}, counted), 256<<10)
	seen := map[string]struct{}{}
	var buf []byte
	lineNo := 0
	requests := 0
	for {
		line, err := br.ReadSlice('\n')
		buf = append(buf[:0], line...)
		for errors.Is(err, bufio.ErrBufferFull) {
			if len(buf) > MaxLineBytes {
				return st, lineError(lineNo+1, "line_too_long", "", fmt.Sprintf("a line is longer than %d MiB", MaxLineBytes>>20))
			}
			line, err = br.ReadSlice('\n')
			buf = append(buf, line...)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			if errors.Is(err, errFileTooLarge) {
				return st, &Error{Status: 413, Type: "invalid_request_error", Code: "file_too_large", Message: fmt.Sprintf("the file is larger than %d MB", fileLimit>>20)}
			}
			return st, &Error{Status: 400, Type: "invalid_request_error", Code: "upload_failed", Message: "reading the file failed: " + err.Error()}
		}
		if len(buf) > 0 {
			lineNo++
			if len(buf) > MaxLineBytes {
				return st, lineError(lineNo, "line_too_long", "", fmt.Sprintf("a line is longer than %d MiB", MaxLineBytes>>20))
			}
			if trimmed := bytes.TrimSpace(buf); len(trimmed) > 0 {
				requests++
				if requests > MaxFileLines {
					return st, &Error{Status: 400, Type: "invalid_request_error", Code: CodeTooManyRequests, Line: lineNo, Message: fmt.Sprintf("the file has more than %d requests", MaxFileLines)}
				}
				url, lerr := checkLine(trimmed, lineNo, seen)
				if lerr != nil {
					return st, lerr
				}
				switch {
				case requests == 1:
					st.Endpoint = url
				case st.MixedLine == 0 && st.Endpoint != url:
					st.Endpoint = ""
					st.MixedLine = lineNo
				}
			}
		}
		if err != nil {
			break
		}
	}
	if requests == 0 {
		return st, &Error{Status: 400, Type: "invalid_request_error", Code: CodeEmptyFile, Message: "the file has no requests"}
	}
	st.Bytes = counted.n
	st.Lines = requests
	return st, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func lineError(line int, code, param, message string) *Error {
	return &Error{Status: 400, Type: "invalid_request_error", Code: code, Param: param, Line: line, Message: fmt.Sprintf("Line %d: %s", line, message)}
}

// checkLine validates one request line and returns its url.
func checkLine(raw []byte, lineNo int, seen map[string]struct{}) (string, *Error) {
	if raw[0] != '{' {
		return "", lineError(lineNo, "invalid_json_line", "", "the line is not a JSON object")
	}
	var l inputLine
	if err := json.Unmarshal(raw, &l); err != nil {
		return "", lineError(lineNo, "invalid_json_line", "", "the line is not valid JSON: "+err.Error())
	}
	var customID string
	if len(l.CustomID) == 0 || json.Unmarshal(l.CustomID, &customID) != nil || customID == "" {
		return "", lineError(lineNo, "missing_custom_id", "custom_id", "custom_id must be a non-empty string")
	}
	if _, dup := seen[customID]; dup {
		return "", lineError(lineNo, "duplicate_custom_id", "custom_id", fmt.Sprintf("custom_id %q is used by an earlier line", customID))
	}
	seen[customID] = struct{}{}
	if l.Method != "POST" {
		return "", lineError(lineNo, "invalid_method", "method", `method must be "POST"`)
	}
	if !slices.Contains(Endpoints, l.URL) {
		return "", lineError(lineNo, "invalid_url", "url", fmt.Sprintf("url must be one of %v", Endpoints))
	}
	if len(l.Body) == 0 || l.Body[0] != '{' {
		return "", lineError(lineNo, "missing_body", "body", "body must be a JSON object")
	}
	var body struct {
		Model  json.RawMessage `json:"model"`
		Stream bool            `json:"stream"`
	}
	if err := json.Unmarshal(l.Body, &body); err != nil {
		return "", lineError(lineNo, "invalid_json_line", "body", "body is not valid: "+err.Error())
	}
	var model string
	if len(body.Model) == 0 || json.Unmarshal(body.Model, &model) != nil || model == "" {
		return "", lineError(lineNo, "missing_model", "body.model", "body.model must be a non-empty string")
	}
	if body.Stream {
		return "", lineError(lineNo, "streaming_not_supported", "body.stream", "a batch cannot stream a response")
	}
	return l.URL, nil
}
