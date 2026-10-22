// A run-owned external engine boundary for browser health-check contracts.
// Only health chat requests are fault-controlled; other engine APIs pass through.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"time"
)

type event struct {
	Seq     int    `json:"seq"`
	Phase   string `json:"phase"`
	AtMS    int64  `json:"at_ms"`
	Request int    `json:"request,omitempty"`
	Method  string `json:"method,omitempty"`
	Path    string `json:"path,omitempty"`
	Query   string `json:"query"`
	Body    any    `json:"body,omitempty"`
	Mode    string `json:"mode,omitempty"`
	HTTP    int    `json:"http_status,omitempty"`
	Message string `json:"message,omitempty"`
}

type audit struct {
	mu   sync.Mutex
	file *os.File
	seq  int
}

func (a *audit) record(e event) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	e.Seq = a.seq
	e.AtMS = time.Now().UnixMilli()
	if err := json.NewEncoder(a.file).Encode(e); err != nil {
		panic(err)
	}
	if err := a.file.Sync(); err != nil {
		panic(err)
	}
	return e.Seq
}

type control struct {
	Mode    string `json:"mode"`
	DelayMS int    `json:"delay_ms"`
}

func readControl(path string) (control, error) {
	var c control
	file, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer file.Close()
	d := json.NewDecoder(io.LimitReader(file, 4096))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	if err = d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return c, errors.New("trailing control content")
	}
	if c.Mode != "ok" && c.Mode != "slow" && c.Mode != "fail" {
		return c, errors.New("invalid control mode")
	}
	if c.DelayMS < 0 || c.DelayMS > 5000 || (c.Mode == "slow" && c.DelayMS != 2000) {
		return c, errors.New("invalid control delay")
	}
	return c, nil
}
func payload(raw []byte, served string) (any, error) {
	var got any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&got); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing request content")
	}
	expected := map[string]any{"model": served, "messages": []any{map[string]any{"role": "user", "content": "1"}}, "stream": false, "max_tokens": json.Number("1")}
	if !reflect.DeepEqual(got, expected) {
		return got, errors.New("health request differs from exact minimal chat contract")
	}
	return got, nil
}
func handler(upstream, served, controlPath string, a *audit) http.Handler {
	target, err := url.Parse(upstream)
	if err != nil {
		panic(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			proxy.ServeHTTP(w, r)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 65537))
		r.Body.Close()
		if err != nil || len(raw) > 65536 {
			http.Error(w, "invalid request body", 400)
			return
		}
		value, validation := payload(raw, served)
		if r.Method != "POST" || r.URL.RawQuery != "" {
			validation = errors.New("invalid health method or query")
		}
		if validation != nil {
			a.record(event{Phase: "rejected", Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: value, HTTP: 400, Message: validation.Error()})
			http.Error(w, validation.Error(), 400)
			return
		}
		c, err := readControl(controlPath)
		if err != nil {
			a.record(event{Phase: "control_failed", Message: err.Error()})
			http.Error(w, "invalid fixture control", 500)
			return
		}
		id := a.record(event{Phase: "started", Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: value, Mode: c.Mode})
		complete := func(phase string, code int, message string) {
			a.record(event{Phase: phase, Request: id, Mode: c.Mode, HTTP: code, Message: message})
		}
		if c.Mode == "slow" {
			timer := time.NewTimer(time.Duration(c.DelayMS) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				complete("cancelled", 0, r.Context().Err().Error())
				return
			case <-timer.C:
			}
		}
		if c.Mode == "fail" {
			complete("completed", 503, "")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			io.WriteString(w, `{"error":{"message":"e2e-health-controlled-failure","type":"server_error"}}`)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", upstream+r.URL.Path, bytes.NewReader(raw))
		if err != nil {
			complete("transport_failed", 0, err.Error())
			http.Error(w, "upstream request", 502)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			complete("transport_failed", 0, err.Error())
			http.Error(w, "upstream transport", 502)
			return
		}
		defer response.Body.Close()
		content, err := io.ReadAll(io.LimitReader(response.Body, 65537))
		if err != nil || len(content) > 65536 {
			complete("transport_failed", 0, "invalid upstream body")
			http.Error(w, "upstream body", 502)
			return
		}
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, err = w.Write(content)
		if err != nil {
			complete("cancelled", 0, err.Error())
			return
		}
		complete("completed", response.StatusCode, "")
	})
}
func main() {
	directory, served := os.Getenv("E2E_HEALTH_DIR"), os.Getenv("E2E_HEALTH_SERVED")
	if directory == "" || served == "" {
		panic("missing run-owned health directory or model")
	}
	file, err := os.OpenFile(filepath.Join(directory, "audit.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		panic(err)
	}
	defer file.Close()
	a := &audit{file: file}
	// The custom entrypoint receives vLLM's positional model arguments.
	// Restore the CLI verb consumed by entrypoint replacement for fake-engine.
	args := append([]string{"serve"}, os.Args[1:]...)
	port, index := "", -1
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--port" {
			port, index = args[i+1], i+1
		}
	}
	if index < 0 {
		panic("missing --port")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	inside := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	args[index] = fmt.Sprint(inside)
	child := exec.Command("/usr/local/bin/fake-engine", args...)
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err = child.Start(); err != nil {
		panic(err)
	}
	a.record(event{Phase: "child_started", Message: fmt.Sprint(child.Process.Pid)})
	childDone := make(chan error, 1)
	go func() { childDone <- child.Wait() }()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	server := &http.Server{Addr: ":" + port, Handler: handler(fmt.Sprintf("http://127.0.0.1:%d", inside), served, filepath.Join(directory, "control.json"), a), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-serverDone:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, err)
		}
	case err = <-childDone:
		a.record(event{Phase: "child_exit", Message: fmt.Sprint(err)})
		return
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(shutdown)
	child.Process.Signal(syscall.SIGTERM)
	select {
	case err = <-childDone:
	case <-shutdown.Done():
		child.Process.Kill()
		err = <-childDone
	}
	a.record(event{Phase: "child_exit", Message: fmt.Sprint(err)})
}
