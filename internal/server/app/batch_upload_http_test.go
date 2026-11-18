package app

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBatchUploadRefusals(t *testing.T) {
	// These refused uploads leave no batch behind, so one real application
	// suffices; each group still owns a separate HTTP server and sockets.
	application, _ := newTestApp(t, "gpuworks-test-password-2026")
	t.Run("early-line", func(t *testing.T) { testBatchUploadRefusalSurvivesAnUnfinishedUpload(t, application) })
	t.Run("bounded", func(t *testing.T) { testBatchUploadRefusalBoundsStalledCancelledAndOversizedBodies(t, application) })
	t.Run("file-size", func(t *testing.T) { testBatchUploadStillRejectsFilesAboveThePublishedLimit(t, application) })
	t.Run("drain-limit", func(t *testing.T) { testBatchUploadRefusalClosesAtTheDrainLimit(t, application) })
	t.Run("keep-alive", func(t *testing.T) { testBatchUploadRefusalKeepsConsumedConnectionsReusable(t, application) })
}

// A browser may still be sending a large file when its first line is rejected.
// Exercise a real socket: ResponseRecorder cannot detect closing a connection
// with an unread upload, which hides the JSON error behind a network failure.
func testBatchUploadRefusalSurvivesAnUnfinishedUpload(t *testing.T, application *App) {
	t.Helper()
	srv := httptest.NewServer(application.http.Handler)
	defer srv.Close()
	token := login(t, srv.URL)
	key, err := application.store.CreateAPIKey("upload-refusal", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, token, code string }{
		{"/api/v1/batches", token, `"code":"BATCH_FILE_INVALID"`},
		{"/v1/files", key, `"code":"invalid_json_line"`},
	} {
		for _, chunked := range []bool{false, true} {
			name := strings.TrimPrefix(tc.path, "/")
			if chunked {
				name += "/chunked"
			}
			t.Run(name, func(t *testing.T) {
				const boundary = "batch-upload-regression"
				head := "--" + boundary + "\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nbatch\r\n--" + boundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"bad.jsonl\"\r\nContent-Type: application/octet-stream\r\n\r\nnot-json\n"
				tail := "\r\n--" + boundary + "--\r\n"
				payload := bytes.Repeat([]byte("x"), 16<<20)
				body := io.MultiReader(strings.NewReader(head), bytes.NewReader(payload), strings.NewReader(tail))
				req, _ := http.NewRequest(http.MethodPost, srv.URL+tc.path, body)
				req.ContentLength = int64(len(head) + len(payload) + len(tail))
				if chunked {
					req.ContentLength = -1
				}
				req.Header.Set("Authorization", "Bearer "+tc.token)
				req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
				conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				if err := req.Write(conn); err != nil {
					t.Fatalf("upload connection failed before its error could be read: %v", err)
				}
				resp, err := http.ReadResponse(bufio.NewReader(conn), req)
				if err != nil {
					t.Fatalf("error response unreadable: %v", err)
				}
				defer resp.Body.Close()
				raw, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != 400 || !bytes.Contains(raw, []byte(tc.code)) {
					t.Fatalf("response = %d %s, err=%v", resp.StatusCode, raw, err)
				}
			})
		}
	}
}

// A refused body is only drained for a finite time and byte count. Clients
// that stop or keep sending beyond that allowance cannot hold the handler.
func testBatchUploadRefusalBoundsStalledCancelledAndOversizedBodies(t *testing.T, application *App) {
	t.Helper()
	finished := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		application.http.Handler.ServeHTTP(w, r)
		if r.Method == http.MethodPost && (r.URL.Path == "/api/v1/batches" || r.URL.Path == "/v1/files") {
			finished <- struct{}{}
		}
	}))
	defer srv.Close()
	token := login(t, srv.URL)
	key, err := application.store.CreateAPIKey("bounded-upload-refusal", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, token, code string }{
		{"/api/v1/batches", token, `"code":"BATCH_FILE_INVALID"`},
		{"/v1/files", key, `"code":"invalid_json_line"`},
	} {
		for _, mode := range []string{"stalled", "stalled-chunked", "cancelled", "oversized"} {
			t.Run(tc.path+"/"+mode, func(t *testing.T) {
				const boundary = "bounded-batch-upload"
				head := "--" + boundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"bad.jsonl\"\r\n\r\nnot-json\n"
				conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(9 * time.Second))
				if mode == "oversized" {
					// Stream far more than the remaining-body allowance without
					// allocating that body. The server must answer before EOF.
					src := &batchUploadRepeater{}
					body := io.MultiReader(strings.NewReader(head), io.LimitReader(src, 240<<20))
					req, _ := http.NewRequest(http.MethodPost, srv.URL+tc.path, body)
					req.ContentLength = -1 // real chunked upload with no declared length
					req.Header.Set("Authorization", "Bearer "+tc.token)
					req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
					written := make(chan error, 1)
					go func() { written <- req.Write(conn) }()
					resp, err := http.ReadResponse(bufio.NewReader(conn), req)
					if err != nil {
						t.Fatalf("bounded error response: %v", err)
					}
					raw, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					_ = conn.Close()
					<-written
					if err != nil || resp.StatusCode != 400 || !bytes.Contains(raw, []byte(tc.code)) || !resp.Close {
						t.Fatalf("response = %d %s close=%v err=%v", resp.StatusCode, raw, resp.Close, err)
					}
					if src.read >= 240<<20 {
						t.Fatal("server consumed the entire oversized stream instead of bounding the drain")
					}
				} else {
					// Advertise more than is sent and leave the TCP stream open.
					length := fmt.Sprintf("Content-Length: %d", 16<<20)
					if mode == "stalled-chunked" {
						length = "Transfer-Encoding: chunked"
					}
					_, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: multipart/form-data; boundary=%s\r\n%s\r\n\r\n", tc.path, tc.token, boundary, length)
					if err != nil {
						t.Fatal(err)
					}
					if mode == "stalled-chunked" {
						_, _ = fmt.Fprintf(conn, "%x\r\n", len(head)+(300<<10))
					}
					if _, err := io.WriteString(conn, head); err != nil {
						t.Fatal(err)
					}
					if _, err := conn.Write(bytes.Repeat([]byte("x"), 300<<10)); err != nil {
						t.Fatal(err)
					}
					if mode == "stalled-chunked" {
						_, _ = io.WriteString(conn, "\r\n")
					}
					if mode == "cancelled" {
						_ = conn.Close()
					} else {
						reader := bufio.NewReader(conn)
						resp, err := http.ReadResponse(reader, nil)
						if err != nil {
							t.Fatalf("stalled error response: %v", err)
						}
						raw, err := io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						if err != nil || resp.StatusCode != 400 || !bytes.Contains(raw, []byte(tc.code)) || !resp.Close {
							t.Fatalf("response = %d %s close=%v err=%v", resp.StatusCode, raw, resp.Close, err)
						}
						// Handler return is insufficient: net/http itself closes
						// and may try to drain the unread chunked body afterwards.
						_ = conn.SetReadDeadline(time.Now().Add(time.Second))
						if _, err := reader.ReadByte(); err != io.EOF {
							t.Fatalf("connection did not finish after refusal: %v", err)
						}
					}
				}
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Fatal("handler did not release the rejected upload")
				}
			})
		}
	}
}

var batchUploadBlankLine = strings.Repeat(" ", 4095) + "\n"

type batchUploadRepeater struct {
	read       int64
	blankLines bool
}

func (r *batchUploadRepeater) Read(p []byte) (int, error) {
	clear(p)
	if r.blankLines {
		const lineSize = 4096
		for at := 0; at < len(p); {
			offset := int((r.read + int64(at)) % lineSize)
			at += copy(p[at:], batchUploadBlankLine[offset:])
		}
	}
	r.read += int64(len(p))
	return len(p), nil
}

func testBatchUploadStillRejectsFilesAboveThePublishedLimit(t *testing.T, application *App) {
	t.Helper()
	srv := httptest.NewServer(application.http.Handler)
	defer srv.Close()
	token := login(t, srv.URL)
	key, err := application.store.CreateAPIKey("upload-size-limit", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, token, code string }{
		{"/api/v1/batches", token, `"code":"BATCH_FILE_TOO_LARGE"`},
		{"/v1/files", key, `"code":"file_too_large"`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			const boundary = "batch-upload-size-limit"
			head := "--" + boundary + "\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nbatch\r\n--" + boundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"large.jsonl\"\r\n\r\n"
			tail := "\r\n--" + boundary + "--\r\n"
			body := io.MultiReader(strings.NewReader(head), io.LimitReader(&batchUploadRepeater{blankLines: true}, 201<<20), strings.NewReader(tail))
			req, _ := http.NewRequest(http.MethodPost, srv.URL+tc.path, body)
			req.ContentLength = int64(len(head) + 201<<20 + len(tail))
			req.Header.Set("Authorization", "Bearer "+tc.token)
			req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
			resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != 413 || !bytes.Contains(raw, []byte(tc.code)) {
				t.Fatalf("response = %d %s err=%v", resp.StatusCode, raw, err)
			}
		})
	}
}

// Exactly exhausting the drain allowance must also bound net/http's later
// request-body Close: an unfinished chunked stream cannot keep it waiting.
func testBatchUploadRefusalClosesAtTheDrainLimit(t *testing.T, application *App) {
	t.Helper()
	srv := httptest.NewServer(application.http.Handler)
	defer srv.Close()
	token := login(t, srv.URL)
	key, err := application.store.CreateAPIKey("upload-exact-drain-limit", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, token, code string }{
		{"/api/v1/batches", token, `"code":"BATCH_REQUEST_INVALID"`},
		{"/v1/files", key, `"code":"invalid_content_type"`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n", tc.path, tc.token, 200<<20)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.CopyN(conn, &batchUploadRepeater{}, 200<<20); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(conn, "\r\n"); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(conn)
			resp, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != 400 || !bytes.Contains(raw, []byte(tc.code)) || !resp.Close {
				t.Fatalf("response = %d %s close=%v err=%v", resp.StatusCode, raw, resp.Close, err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := reader.ReadByte(); err != io.EOF {
				t.Fatalf("connection did not finish at its drain limit: %v", err)
			}
		})
	}
}

func testBatchUploadRefusalKeepsConsumedConnectionsReusable(t *testing.T, application *App) {
	t.Helper()
	srv := httptest.NewServer(application.http.Handler)
	defer srv.Close()
	token := login(t, srv.URL)
	key, err := application.store.CreateAPIKey("upload-keep-alive", 0)
	if err != nil {
		t.Fatal(err)
	}
	type connection struct {
		net.Conn
		reader      *bufio.Reader
		path, token string
	}
	var connections []connection
	for _, tc := range []struct{ path, next, token string }{
		{"/api/v1/batches", "/api/v1/batches/dictionary", token},
		{"/v1/files", "/v1/files", key},
	} {
		conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		req, _ := http.NewRequest(http.MethodPost, srv.URL+tc.path, strings.NewReader("bad content type"))
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("Content-Type", "application/json")
		if err := req.Write(conn); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(conn)
		resp, err := http.ReadResponse(reader, req)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 400 || resp.Close {
			t.Fatalf("consumed upload response = %d %s close=%v err=%v", resp.StatusCode, raw, resp.Close, err)
		}
		connections = append(connections, connection{conn, reader, tc.next, tc.token})
	}
	// One wait tests both adapters after their rejected-body read deadline.
	time.Sleep(5500 * time.Millisecond)
	for _, conn := range connections {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+conn.path, nil)
		req.Header.Set("Authorization", "Bearer "+conn.token)
		if err := req.Write(conn); err != nil {
			t.Fatalf("reuse %s: %v", conn.path, err)
		}
		resp, err := http.ReadResponse(conn.reader, req)
		if err != nil {
			t.Fatalf("reuse %s: %v", conn.path, err)
		}
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("reuse %s = %d %s err=%v", conn.path, resp.StatusCode, raw, err)
		}
	}
}
