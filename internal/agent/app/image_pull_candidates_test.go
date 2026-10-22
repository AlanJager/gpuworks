package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/agent/registryprobe"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// candidateDriver pulls per reference: a ref in pullErrs fails, every other
// one succeeds. It records what each pull was given and every tag.
type candidateDriver struct {
	*imagePullTestDriver
	mu      sync.Mutex
	pullErr map[string]error
	tagErr  map[string]error
	pulled  []string
	tagged  [][2]string
	auth    map[string]proto.SourceAuth
	proxies map[string]string
}

func newCandidateDriver() *candidateDriver {
	return &candidateDriver{imagePullTestDriver: &imagePullTestDriver{tagImage: true}, pullErr: map[string]error{}, tagErr: map[string]error{}, auth: map[string]proto.SourceAuth{}, proxies: map[string]string{}}
}

func (d *candidateDriver) Name() string { return "candidate-test" }

func (d *candidateDriver) PullImage(ctx context.Context, ref string, onLine func(string)) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pulled = append(d.pulled, ref)
	if a, ok := runtime.RegistryAuthFor(ctx, ref); ok {
		d.auth[ref] = a
	}
	d.proxies[ref] = runtime.PullProxy(ctx)
	if onLine != nil {
		onLine("pulling " + ref)
	}
	return d.pullErr[ref]
}

func (d *candidateDriver) TagImage(_ context.Context, source, canonical string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tagged = append(d.tagged, [2]string{source, canonical})
	return d.tagErr[source]
}

// pullRun starts an image.pull and returns every image.node status up to the
// terminal one.
func pullRun(t *testing.T, n *NodeRuntime, raw string) []proto.ImageNodeStatus {
	t.Helper()
	statuses := make(chan proto.ImageNodeStatus, 256)
	n.send = func(kind string, data any) error {
		if kind == proto.TStatusImageNode {
			statuses <- data.(proto.ImageNodeStatus)
		}
		return nil
	}
	if _, err := n.rpcImagePull(context.Background(), json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
	var got []proto.ImageNodeStatus
	deadline := time.After(5 * time.Second)
	for {
		select {
		case st := <-statuses:
			got = append(got, st)
			if st.State == "present" || st.State == "failed" {
				return got
			}
		case <-deadline:
			t.Fatalf("no terminal status; got %+v", got)
		}
	}
}

func messages(statuses []proto.ImageNodeStatus) string {
	var all []string
	for _, st := range statuses {
		all = append(all, st.Message)
	}
	return strings.Join(all, "\n")
}

const pullCandidatesParams = `{"effective_ref":"a.example/org/repo:v1","canonical_ref":"ghcr.io/org/repo:v1","transfer_id":"t1","candidates":[{"ref":"a.example/org/repo:v1"},{"ref":"b.example/org/repo:v1"}]}`

func TestRPCImagePullWalksCandidatesAndTagsTheOneThatWorked(t *testing.T) {
	d := newCandidateDriver()
	d.pullErr["a.example/org/repo:v1"] = errors.New("registry a down")
	n := NewNodeRuntime(slog.Default(), d, t.TempDir())
	got := pullRun(t, n, pullCandidatesParams)
	last := got[len(got)-1]
	if last.State != "present" || last.Source != "b.example/org/repo:v1" || last.ImageRef != "ghcr.io/org/repo:v1" || last.TransferID != "t1" {
		t.Fatalf("terminal status = %+v", last)
	}
	if strings.Join(d.pulled, ",") != "a.example/org/repo:v1,b.example/org/repo:v1" {
		t.Fatalf("pulled %v", d.pulled)
	}
	if len(d.tagged) != 1 || d.tagged[0] != [2]string{"b.example/org/repo:v1", "ghcr.io/org/repo:v1"} {
		t.Fatalf("tagged %v, want the successful ref tagged to the canonical ref", d.tagged)
	}
	if last.ImageID != "sha256:test" {
		t.Fatalf("image id = %q", last.ImageID)
	}
	if !strings.Contains(last.Message, "image source failed: a.example/org/repo:v1: registry a down") {
		t.Fatalf("the failed source is not visible:\n%s", messages(got))
	}
}

func TestRPCImagePullCandidateThatIsAlreadyTheCanonicalRefIsNotTagged(t *testing.T) {
	d := newCandidateDriver()
	n := NewNodeRuntime(slog.Default(), d, t.TempDir())
	got := pullRun(t, n, `{"effective_ref":"ghcr.io/org/repo:v1","canonical_ref":"ghcr.io/org/repo:v1","candidates":[{"ref":"ghcr.io/org/repo:v1"}]}`)
	if got[len(got)-1].State != "present" || len(d.tagged) != 0 {
		t.Fatalf("status=%+v tagged=%v", got[len(got)-1], d.tagged)
	}
}

func TestRPCImagePullCandidatesAllFailing(t *testing.T) {
	d := newCandidateDriver()
	d.pullErr["a.example/org/repo:v1"] = errors.New("denied")
	d.pullErr["b.example/org/repo:v1"] = errors.New("timeout")
	n := NewNodeRuntime(slog.Default(), d, t.TempDir())
	got := pullRun(t, n, pullCandidatesParams)
	last := got[len(got)-1]
	if last.State != "failed" || last.Message != "all image sources failed: a.example/org/repo:v1: denied | b.example/org/repo:v1: timeout" {
		t.Fatalf("terminal status = %+v", last)
	}
	if len(d.tagged) != 0 {
		t.Fatalf("tagged %v after every source failed", d.tagged)
	}
}

func TestRPCImagePullCandidateTagFailureFallsThroughToTheNext(t *testing.T) {
	d := newCandidateDriver()
	d.tagErr["a.example/org/repo:v1"] = errors.New("tag denied")
	n := NewNodeRuntime(slog.Default(), d, t.TempDir())
	got := pullRun(t, n, pullCandidatesParams)
	last := got[len(got)-1]
	if last.State != "present" || last.Source != "b.example/org/repo:v1" {
		t.Fatalf("terminal status = %+v", last)
	}
	if !strings.Contains(got[len(got)-1].Message, "tag canonical image: tag denied") {
		t.Fatalf("tag failure not visible:\n%s", messages(got))
	}
}

func TestRPCImagePullCandidatesWithoutTaggingSupportFails(t *testing.T) {
	d := &imagePullNoTagDriver{base: &imagePullTestDriver{}}
	n := NewNodeRuntime(slog.Default(), d, t.TempDir())
	got := pullRun(t, n, pullCandidatesParams)
	if last := got[len(got)-1]; last.State != "failed" || !strings.Contains(last.Message, "does not support canonical image tagging") {
		t.Fatalf("terminal status = %+v", last)
	}
}

func TestRPCImagePullRejectsDigestCandidates(t *testing.T) {
	n := NewNodeRuntime(slog.Default(), newCandidateDriver(), t.TempDir())
	_, err := n.rpcImagePull(context.Background(), json.RawMessage(`{"effective_ref":"a/b:v1","canonical_ref":"c/d:v1","candidates":[{"ref":"a/b:v1"},{"ref":"x/y@sha256:abc"}]}`))
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("err = %v", err)
	}
}

type hostsTransport map[string]bool // host -> reachable

func (h hostsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !h[req.URL.Host] {
		return nil, errors.New("connection refused")
	}
	return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
}

func TestRPCImagePullRanksMarkedCandidatesAndReportsTheOrder(t *testing.T) {
	d := newCandidateDriver()
	n := NewNodeRuntime(slog.Default(), d, t.TempDir())
	n.puller = imagepull.New(imagepull.Config{Log: slog.Default(), Prober: registryprobe.New(
		registryprobe.WithTimeout(time.Second),
		registryprobe.WithTransport(func(registryprobe.Route) http.RoundTripper { return hostsTransport{"b.example": true} }),
	)})
	got := pullRun(t, n, `{"effective_ref":"a.example/org/repo:v1","canonical_ref":"ghcr.io/org/repo:v1","candidates":[{"ref":"a.example/org/repo:v1","rank":true},{"ref":"b.example/org/repo:v1","rank":true}]}`)
	if last := got[len(got)-1]; last.State != "present" || last.Source != "b.example/org/repo:v1" {
		t.Fatalf("terminal status = %+v", last)
	}
	if strings.Join(d.pulled, ",") != "b.example/org/repo:v1" {
		t.Fatalf("pulled %v, want the reachable registry only", d.pulled)
	}
	if last := got[len(got)-1].Message; strings.Count(last, "image sources by probe:") != 1 || !strings.Contains(last, "a.example (unreachable)") {
		t.Fatalf("order not reported once:\n%s", messages(got))
	}
}

func TestRegistryLoginsFromStateSyncReachPullsAndAreReplacedEachSync(t *testing.T) {
	d := newCandidateDriver()
	n := NewNodeRuntime(slog.Default(), d, t.TempDir())
	ctx := context.Background()
	n.OnStateSync(ctx, proto.DesiredState{RegistryAuth: []proto.SourceAuth{{Host: "a.example", Username: "robot", Secret: "s3cret-token"}}})
	got := pullRun(t, n, pullCandidatesParams)
	if last := got[len(got)-1]; last.State != "present" {
		t.Fatalf("terminal status = %+v", last)
	}
	if a := d.auth["a.example/org/repo:v1"]; a.Username != "robot" || a.Secret != "s3cret-token" {
		t.Fatalf("pull of a.example got login %+v", a)
	}
	if _, leaked := d.auth["b.example/org/repo:v1"]; leaked {
		t.Fatal("a login for a.example was handed to a pull from b.example")
	}
	if strings.Contains(messages(got), "s3cret-token") {
		t.Fatal("the secret appears in a status message")
	}

	// A sync without logins withdraws them.
	n.OnStateSync(ctx, proto.DesiredState{})
	d.auth = map[string]proto.SourceAuth{}
	d.pulled = nil
	pullRun(t, n, pullCandidatesParams)
	if len(d.auth) != 0 {
		t.Fatalf("a withdrawn login was still used: %+v", d.auth)
	}
}

func TestRegistryLoginsNeverPrint(t *testing.T) {
	n := NewNodeRuntime(slog.Default(), newCandidateDriver(), t.TempDir())
	n.setRegistryAuth([]proto.SourceAuth{{Host: "a.example", Username: "robot", Secret: "s3cret-token"}})
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("state", "auth", n.registryAuth)
	for _, rendered := range []string{
		fmt.Sprintf("%v", n.registryAuth), fmt.Sprintf("%+v", n.registryAuth), fmt.Sprintf("%#v", n.registryAuth),
		fmt.Sprint(n.registryAuth), buf.String(),
	} {
		if strings.Contains(rendered, "s3cret-token") {
			t.Fatalf("secret printed: %s", rendered)
		}
		if !strings.Contains(rendered, "robot@a.example") {
			t.Fatalf("rendering lost the registry and user: %s", rendered)
		}
	}
	// The pull itself still gets the real values, as a copy.
	got := n.currentRegistryAuth()
	if len(got) != 1 || got[0].Secret != "s3cret-token" {
		t.Fatalf("currentRegistryAuth = %+v", got)
	}
	got[0].Secret = "mutated"
	if n.currentRegistryAuth()[0].Secret != "s3cret-token" {
		t.Fatal("callers can mutate the stored logins")
	}
}

func TestRPCImagePullCandidatesUseTheNodeProxyForNonDockerRuntimes(t *testing.T) {
	d := newCandidateDriver()
	n := NewNodeRuntime(slog.Default(), d, t.TempDir())
	n.setNetwork(proto.NetworkConfig{HTTPSProxy: "http://node.proxy:3128"})
	pullRun(t, n, pullCandidatesParams)
	if d.proxies["a.example/org/repo:v1"] != "http://node.proxy:3128" {
		t.Fatalf("pull proxy = %q", d.proxies["a.example/org/repo:v1"])
	}
}
