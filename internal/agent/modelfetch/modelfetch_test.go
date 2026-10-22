package modelfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/download"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

const (
	repo   = "org/model"
	commit = "25d1d1fa49de445ab1f3a41f3537d7dad222826e"
)

// hub is a Hugging Face fixture: the tree listing and resolve endpoints as
// the real hub answers them, with resolve answering a weight file by a
// redirect to its CDN (here, a path on the same server).
type hub struct {
	*httptest.Server
	files map[string][]byte
	// listStatus, when set, is the status the tree listing answers.
	listStatus int
	// fileStatus, when set, is the status every resolve answers.
	fileStatus int
	// stall makes every transfer of these files deliver its first half and
	// then nothing (and, once resumed, nothing at all).
	stall map[string]bool

	mu       sync.Mutex
	starts   map[string][]int64 // first byte of each content request, per file
	resolves int
}

func newHub(t *testing.T, files map[string][]byte) *hub {
	t.Helper()
	h := &hub{files: files, starts: map[string][]int64{}}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Close)
	return h
}

func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/api/models/"+repo+"/tree/"):
		if h.listStatus != 0 {
			http.Error(w, `{"error":"Repository not found"}`, h.listStatus)
			return
		}
		if r.URL.Query().Get("recursive") != "true" {
			http.Error(w, "fixture expects a recursive listing", http.StatusBadRequest)
			return
		}
		type item struct {
			Type string `json:"type"`
			Path string `json:"path"`
			Size int64  `json:"size"`
		}
		names := make([]string, 0, len(h.files))
		for name := range h.files {
			names = append(names, name)
		}
		sort.Strings(names)
		items := []item{{Type: "directory", Path: "docs"}}
		for _, name := range names {
			items = append(items, item{Type: "file", Path: name, Size: int64(len(h.files[name]))})
		}
		_ = json.NewEncoder(w).Encode(items)
	case strings.HasPrefix(path, "/"+repo+"/resolve/"):
		h.mu.Lock()
		h.resolves++
		h.mu.Unlock()
		if h.fileStatus != 0 {
			http.Error(w, "unavailable", h.fileStatus)
			return
		}
		name := path[strings.Index(path, "/resolve/")+len("/resolve/"):]
		_, name, _ = strings.Cut(name, "/")
		if strings.HasSuffix(name, ".safetensors") {
			http.Redirect(w, r, "/cdn/"+name, http.StatusFound)
			return
		}
		h.content(w, r, name)
	case strings.HasPrefix(path, "/cdn/"):
		h.content(w, r, strings.TrimPrefix(path, "/cdn/"))
	default:
		http.NotFound(w, r)
	}
}

func (h *hub) content(w http.ResponseWriter, r *http.Request, name string) {
	data, ok := h.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	spec := r.Header.Get("Range")
	if spec == "bytes=0-0" {
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
		return
	}
	start := int64(0)
	if from, ok := strings.CutPrefix(spec, "bytes="); ok {
		first, _, _ := strings.Cut(from, "-")
		start, _ = strconv.ParseInt(first, 10, 64)
	}
	h.mu.Lock()
	h.starts[name] = append(h.starts[name], start)
	h.mu.Unlock()
	if !h.stall[name] {
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(data))-start, 10))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
	w.WriteHeader(http.StatusPartialContent)
	if half := int64(len(data) / 2); start < half {
		_, _ = w.Write(data[start:half])
		w.(http.Flusher).Flush()
	}
	<-r.Context().Done()
}

func (h *hub) startsOf(name string) []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int64(nil), h.starts[name]...)
}

func (h *hub) host() string { return Host(h.URL) }

// cooler is a Cooler that records what it was told.
type cooler struct {
	mu     sync.Mutex
	cooled map[string]bool
}

func (c *cooler) Cool(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cooled == nil {
		c.cooled = map[string]bool{}
	}
	c.cooled[host] = true
}

func (c *cooler) Cooled(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cooled[host]
}

// deadAddress is an address nothing listens on.
func deadAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "http://" + listener.Addr().String()
	_ = listener.Close()
	return address
}

func modelFiles() map[string][]byte {
	return map[string][]byte{
		"config.json":       []byte(`{"model_type":"test"}`),
		"model.safetensors": bytes.Repeat([]byte("weights!"), 4096),
	}
}

type run struct {
	switches []Switch
}

func newFetcher(t *testing.T, cool Cooler, r *run, bases ...string) *Fetcher {
	t.Helper()
	candidates := make([]proto.ModelFetchCandidate, 0, len(bases))
	for i, base := range bases {
		candidates = append(candidates, proto.ModelFetchCandidate{Base: base, Upstream: i == len(bases)-1})
	}
	f, err := New(Config{
		Source: modelrepo.SourceHF, Repo: repo, Candidates: candidates, Cooler: cool,
		Retry:        RetryPolicy{MaxAttempts: 3},
		StallTimeout: 300 * time.Millisecond,
		OnSwitch:     func(s Switch) { r.switches = append(r.switches, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// fetchAll lists and fetches the whole repository into dir, as the agent does.
func fetchAll(t *testing.T, f *Fetcher, revision, dir string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	files, err := f.List(ctx, revision)
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := f.Fetch(ctx, File{Path: file.Path, Revision: revision, Dest: filepath.Join(dir, file.Path), Concurrency: 1}, nil); err != nil {
			return err
		}
	}
	return nil
}

func assertFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %d bytes, err=%v; want %d bytes", name, len(got), err, len(want))
		}
	}
}

func TestAnAddressThatRefusesConnectionsIsSkippedAndCooled(t *testing.T) {
	files := modelFiles()
	upstream := newHub(t, files)
	dead := deadAddress(t)
	cool, r, dir := &cooler{}, &run{}, t.TempDir()
	f := newFetcher(t, cool, r, dead, upstream.URL)

	if err := fetchAll(t, f, commit, dir); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, files)
	if !cool.Cooled(Host(dead)) || cool.Cooled(upstream.host()) {
		t.Fatalf("cooled = %v, want only the dead address", cool.cooled)
	}
	if len(r.switches) != 1 || r.switches[0].From != Host(dead) || r.switches[0].To != upstream.host() {
		t.Fatalf("switches = %+v", r.switches)
	}
	if f.Host() != upstream.host() {
		t.Fatalf("the fetch ends at %q", f.Host())
	}
}

// A mirror that does not have the repository (or the commit) answers the
// listing with 404; the next address lists it.
func TestAMirrorWithoutTheRepositoryIsSkipped(t *testing.T) {
	files := modelFiles()
	mirror, upstream := newHub(t, files), newHub(t, files)
	mirror.listStatus = http.StatusNotFound
	cool, r, dir := &cooler{}, &run{}, t.TempDir()
	if err := fetchAll(t, newFetcher(t, cool, r, mirror.URL, upstream.URL), commit, dir); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, files)
	if len(r.switches) != 1 || !strings.Contains(r.switches[0].Reason, "http 404") {
		t.Fatalf("switches = %+v", r.switches)
	}
	if mirror.resolves != 0 {
		t.Fatalf("the mirror that could not list was asked for %d files", mirror.resolves)
	}
}

// A file the mirror answers with an error status is fetched from the next
// address at once, without retrying the mirror.
func TestAnErrorStatusMovesToTheNextAddressWithoutRetrying(t *testing.T) {
	files := modelFiles()
	mirror, upstream := newHub(t, files), newHub(t, files)
	mirror.fileStatus = http.StatusBadGateway
	cool, r, dir := &cooler{}, &run{}, t.TempDir()
	if err := fetchAll(t, newFetcher(t, cool, r, mirror.URL, upstream.URL), commit, dir); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, files)
	if mirror.resolves != 1 {
		t.Fatalf("mirror asked %d times, want once", mirror.resolves)
	}
	if !cool.Cooled(mirror.host()) {
		t.Fatal("the failing mirror was not cooled")
	}
}

// Two stalls in a row on one address move a pinned file to the next address,
// which continues the bytes already on disk instead of starting over.
func TestTwoStallsMoveAPinnedFileOnAndResume(t *testing.T) {
	files := modelFiles()
	mirror, upstream := newHub(t, files), newHub(t, files)
	mirror.stall = map[string]bool{"model.safetensors": true}
	cool, r, dir := &cooler{}, &run{}, t.TempDir()
	if err := fetchAll(t, newFetcher(t, cool, r, mirror.URL, upstream.URL), commit, dir); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, files)
	if got := mirror.startsOf("model.safetensors"); len(got) != StallsBeforeSwitch {
		t.Fatalf("mirror transfers = %v, want %d", got, StallsBeforeSwitch)
	}
	half := int64(len(files["model.safetensors"]) / 2)
	if got := upstream.startsOf("model.safetensors"); len(got) != 1 || got[0] != half {
		t.Fatalf("upstream transfers started at %v, want one at %d", got, half)
	}
	if len(r.switches) != 1 || !strings.Contains(r.switches[0].Reason, "stalled 2 times") {
		t.Fatalf("switches = %+v", r.switches)
	}
}

// A file not pinned to a commit starts over at the next address: under a
// branch name another address may serve other bytes.
func TestAnUnpinnedFileStartsOverAtTheNextAddress(t *testing.T) {
	files := modelFiles()
	mirror, upstream := newHub(t, files), newHub(t, files)
	mirror.stall = map[string]bool{"model.safetensors": true}
	cool, r, dir := &cooler{}, &run{}, t.TempDir()
	if err := fetchAll(t, newFetcher(t, cool, r, mirror.URL, upstream.URL), "main", dir); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, files)
	if got := upstream.startsOf("model.safetensors"); len(got) != 1 || got[0] != 0 {
		t.Fatalf("upstream transfers started at %v, want one from byte 0", got)
	}
}

func TestEveryAddressFailingNamesEachReason(t *testing.T) {
	files := modelFiles()
	mirror, upstream := newHub(t, files), newHub(t, files)
	mirror.fileStatus = http.StatusServiceUnavailable
	upstream.fileStatus = http.StatusForbidden
	err := fetchAll(t, newFetcher(t, &cooler{}, &run{}, mirror.URL, upstream.URL), commit, t.TempDir())
	if err == nil {
		t.Fatal("expected a failure")
	}
	for _, want := range []string{"every source failed", mirror.host() + " failed", "status 503", upstream.host() + " failed", "status 403"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestAListingNoAddressAnswersFails(t *testing.T) {
	mirror := newHub(t, modelFiles())
	mirror.listStatus = http.StatusUnauthorized
	_, err := newFetcher(t, &cooler{}, &run{}, mirror.URL).List(context.Background(), commit)
	if err == nil || !strings.Contains(err.Error(), "list files: "+mirror.host()+" failed") || !strings.Contains(err.Error(), "http 401") {
		t.Fatalf("err = %v", err)
	}
}

// Addresses that failed recently are tried after every other one.
func TestCooledAddressesAreTriedLast(t *testing.T) {
	files := modelFiles()
	mirror, upstream := newHub(t, files), newHub(t, files)
	cool := &cooler{}
	cool.Cool(mirror.host())
	f := newFetcher(t, cool, &run{}, mirror.URL, upstream.URL)
	if f.Host() != upstream.host() {
		t.Fatalf("first address = %q, want the uncooled %q", f.Host(), upstream.host())
	}
	if err := fetchAll(t, f, commit, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if mirror.resolves != 0 {
		t.Fatal("the cooled mirror was used although the upstream served")
	}
}

// The node's own disk failing is not the address's fault: the fetch stops
// there instead of cooling a good address and trying the next.
func TestALocalFailureDoesNotMoveOn(t *testing.T) {
	files := modelFiles()
	mirror, upstream := newHub(t, files), newHub(t, files)
	cool, r := &cooler{}, &run{}
	f := newFetcher(t, cool, r, mirror.URL, upstream.URL)
	ctx := context.Background()
	if _, err := f.List(ctx, commit); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := f.Fetch(ctx, File{Path: "config.json", Revision: commit, Dest: filepath.Join(blocked, "config.json"), Concurrency: 1}, nil)
	if err == nil || len(r.switches) != 0 || cool.Cooled(mirror.host()) {
		t.Fatalf("err = %v, switches = %+v, cooled = %v", err, r.switches, cool.cooled)
	}
}

// A transient transport failure is retried at the same address before the
// fetch gives up on it.
func TestATransientFailureIsRetriedAtTheSameAddress(t *testing.T) {
	data := []byte(`{"model_type":"test"}`)
	var mu sync.Mutex
	transfers := 0
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			_, _ = w.Write([]byte(`[{"type":"file","path":"config.json","size":21}]`))
			return
		}
		if r.Header.Get("Range") == "bytes=0-0" {
			http.ServeContent(w, r, "config.json", time.Time{}, bytes.NewReader(data))
			return
		}
		mu.Lock()
		transfers++
		attempt := transfers
		mu.Unlock()
		if attempt == 1 {
			// A short body: the client sees an unexpected EOF.
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			_, _ = w.Write(data[:5])
			return
		}
		http.ServeContent(w, r, "config.json", time.Time{}, bytes.NewReader(data))
	}))
	defer flaky.Close()
	r, dir := &run{}, t.TempDir()
	if err := fetchAll(t, newFetcher(t, &cooler{}, r, flaky.URL), commit, dir); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, map[string][]byte{"config.json": data})
	if transfers != 2 || len(r.switches) != 0 {
		t.Fatalf("transfers = %d, switches = %+v", transfers, r.switches)
	}
}

func TestNewRefusesAnEmptyCandidateList(t *testing.T) {
	if _, err := New(Config{Source: modelrepo.SourceHF, Repo: repo}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestIsCommit(t *testing.T) {
	for revision, want := range map[string]bool{commit: true, "abc1234": true, "main": false, "v1.0": false, "": false, "ABC1234": false} {
		if isCommit(revision) != want {
			t.Errorf("isCommit(%q) = %v", revision, !want)
		}
	}
}

func TestIsTransientKeepsStatusesOut(t *testing.T) {
	if IsTransient(&download.StatusError{Code: 503, Detail: "download: unexpected probe status 503"}) {
		t.Fatal("an error status is the address's answer, not a transport blip")
	}
	if !IsTransient(fmt.Errorf("read: %w", context.DeadlineExceeded)) {
		t.Fatal("a client timeout is transient")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// The last address is all there is: it rides out a minute-scale outage
// (here nine refused TLS handshakes) as a single address always did, while
// an address with another behind it is given up on sooner.
func TestTheLastAddressGetsTheOldPatience(t *testing.T) {
	files := map[string][]byte{"config.json": []byte(`{"model_type":"test"}`)}
	for name, tc := range map[string]struct {
		mirrorFirst bool
		wantSwitch  bool
	}{
		"only address":            {false, false},
		"address with a fallback": {true, true},
	} {
		t.Run(name, func(t *testing.T) {
			flaky, upstream := newHub(t, files), newHub(t, files)
			var mu sync.Mutex
			refusals := map[string]int{}
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				if request.URL.Host == flaky.host() && strings.Contains(request.URL.Path, "/resolve/") && refusals[request.URL.Host] < 9 {
					refusals[request.URL.Host]++
					return nil, fmt.Errorf("net/http: TLS handshake timeout")
				}
				return http.DefaultTransport.RoundTrip(request)
			})}
			bases := []string{flaky.URL}
			if tc.mirrorFirst {
				bases = append(bases, upstream.URL)
			}
			candidates := []proto.ModelFetchCandidate{}
			for _, base := range bases {
				candidates = append(candidates, proto.ModelFetchCandidate{Base: base})
			}
			r := &run{}
			f, err := New(Config{Source: modelrepo.SourceHF, Repo: repo, Candidates: candidates,
				Client:   func(string) (*http.Client, error) { return client, nil },
				Retry:    RetryPolicy{MaxAttempts: 6, LastAttempts: 16},
				OnSwitch: func(s Switch) { r.switches = append(r.switches, s) }})
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := fetchAll(t, f, commit, dir); err != nil {
				t.Fatal(err)
			}
			assertFiles(t, dir, files)
			if got := len(r.switches) > 0; got != tc.wantSwitch {
				t.Fatalf("switched = %v (%+v), want %v", got, r.switches, tc.wantSwitch)
			}
			if !tc.wantSwitch && refusals[flaky.host()] != 9 {
				t.Fatalf("the only address was refused %d times before it served", refusals[flaky.host()])
			}
		})
	}
}

// At the last address a stall is retried, not counted towards a switch.
func TestTheLastAddressRetriesStalls(t *testing.T) {
	data := bytes.Repeat([]byte("weights!"), 4096)
	var mu sync.Mutex
	stalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			fmt.Fprintf(w, `[{"type":"file","path":"model.bin","size":%d}]`, len(data))
			return
		}
		if r.Header.Get("Range") != "bytes=0-0" {
			mu.Lock()
			stalls++
			stalled := stalls <= 3
			mu.Unlock()
			if stalled {
				<-r.Context().Done()
				return
			}
		}
		http.ServeContent(w, r, "model.bin", time.Time{}, bytes.NewReader(data))
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := fetchAll(t, newFetcher(t, &cooler{}, &run{}, server.URL), commit, dir); err == nil {
		t.Fatal("three attempts allowed and three stalls: expected a failure")
	}
	mu.Lock()
	stalls = 0
	mu.Unlock()
	f, err := New(Config{Source: modelrepo.SourceHF, Repo: repo, Candidates: []proto.ModelFetchCandidate{{Base: server.URL, Upstream: true}},
		Retry: RetryPolicy{MaxAttempts: 3, LastAttempts: 5}, StallTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := fetchAll(t, f, commit, dir); err != nil {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("the last address gave up after %d stalls: %v", stalls, err)
	}
	assertFiles(t, dir, map[string][]byte{"model.bin": data})
}
