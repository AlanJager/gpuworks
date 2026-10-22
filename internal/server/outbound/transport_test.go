package outbound_test

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/outbound"
)

func TestRequestsFollowCurrentSettingsWithoutChangingDefaultTransport(t *testing.T) {
	var setting atomic.Value
	setting.Store(proto.NetworkConfig{})
	original := http.DefaultTransport
	tr := outbound.Transport(func() (proto.NetworkConfig, error) { return setting.Load().(proto.NetworkConfig), nil })
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	for _, want := range []string{"first", "second"} {
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodConnect || r.Host != "8.8.8.8:80" {
				t.Errorf("proxy request=%s %s", r.Method, r.Host)
				w.WriteHeader(400)
				return
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
			request, err := http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				t.Error(err)
				return
			}
			request.Body.Close()
			_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(want), want)
		}))
		setting.Store(proto.NetworkConfig{HTTPProxy: proxy.URL})
		response, err := client.Get("http://8.8.8.8/tags")
		if err != nil {
			proxy.Close()
			t.Fatal(err)
		}
		b, _ := io.ReadAll(response.Body)
		response.Body.Close()
		proxy.Close()
		if string(b) != want {
			t.Fatalf("body=%q want=%q", b, want)
		}
	}
	for _, bad := range []proto.NetworkConfig{{HTTPProxy: "http://alice:secret@"}, {HTTPSProxy: "ftp://alice:secret@proxy.invalid"}} {
		setting.Store(bad)
		_, err := client.Get("http://8.8.8.8/tags")
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid setting must fail without exposing credentials: %v", err)
		}
	}
	if http.DefaultTransport != original {
		t.Fatal("global transport changed")
	}
}

func TestSettingReadFailureDoesNotReachNetwork(t *testing.T) {
	var hits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer origin.Close()
	tr := outbound.Transport(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, fmt.Errorf("storage failed") })
	defer tr.CloseIdleConnections()
	_, err := (&http.Client{Transport: tr}).Get(origin.URL)
	if err == nil || hits.Load() != 0 {
		t.Fatalf("error=%v network requests=%d", err, hits.Load())
	}
}

// The management node's no_proxy keeps its requests to the hosts it lists
// off the proxy.
func TestNoProxyHostsAreReachedDirectly(t *testing.T) {
	tr := outbound.Transport(func() (proto.NetworkConfig, error) {
		return proto.NetworkConfig{HTTPProxy: "http://proxy.invalid:3128", HTTPSProxy: "http://proxy.invalid:3128", NoProxy: "files.corp"}, nil
	})
	defer tr.CloseIdleConnections()
	for target, direct := range map[string]bool{"http://files.corp/m/": true, "https://huggingface.co/api": false} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		got, err := tr.Proxy(req)
		if err != nil || (got == nil) != direct {
			t.Errorf("%s: proxy=%v err=%v want direct=%v", target, got, err, direct)
		}
	}
}
