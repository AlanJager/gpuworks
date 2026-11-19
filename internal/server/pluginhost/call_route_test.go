package pluginhost

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

// signWith signs a principal for route action under the key DoSigned
// hands it, and records each key.
func signWith(action string, keys *[][]byte) Signer {
	return func(_ extapi.Digest, key []byte, h http.Header) error {
		*keys = append(*keys, append([]byte(nil), key...))
		p := &extapi.Principal{Kind: extapi.PrincipalPublic, RequestID: "req-1", RouteAction: action, Route: "/x",
			Exp: extapi.PrincipalExpiry(time.Now(), 0)}
		hdr, mac, err := extapi.SignPrincipal(key, p, "")
		if err != nil {
			return err
		}
		h.Set(extapi.HeaderPrincipal, hdr)
		h.Set(extapi.HeaderPrincipalMac, mac)
		h.Set(extapi.HeaderFeatureAccess, "")
		return nil
	}
}

// A signed call is signed for the process that serves it: an on-demand
// plugin started again after going idle has a new key, and the call that
// starts it verifies under that key.
func TestSignedCallsUseTheKeyOfTheProcessThatServesThem(t *testing.T) {
	sp := &fakeSpawner{config: func(int) pluginsdk.Config {
		c := sdkConfig("acme_driver")
		c.Handler = pluginsdk.RequirePrincipal("acme_driver.x", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "ok")
		}))
		return c
	}}
	timing := fastTiming()
	timing.IdleStop = 30 * time.Millisecond
	h, _ := newTestHost(t, sp, nil, timing)
	_ = h.Start(onDemandSpec(t, "acme_driver"))
	waitStatus(t, h, "acme_driver", inState(StateIdle))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var keys [][]byte
	for n := range 2 {
		req, _ := http.NewRequest(http.MethodGet, "/r/x", nil)
		resp, err := h.DoSigned(ctx, "acme_driver", req, Class{}, signWith("acme_driver.x", &keys))
		if err != nil {
			t.Fatalf("call %d: %v", n, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "ok" {
			t.Fatalf("call %d = %d %s", n, resp.StatusCode, body)
		}
		if req.Header.Get(extapi.HeaderPrincipal) != "" {
			t.Fatal("DoSigned signed the caller's request instead of a copy")
		}
		waitStatus(t, h, "acme_driver", inState(StateIdle))
	}
	if sp.count() != 2 || len(keys) != 2 || string(keys[0]) == string(keys[1]) {
		t.Fatalf("%d spawns, keys %x: want two spawns with two keys", sp.count(), keys)
	}

	// A principal for another route is refused by the plugin, and a
	// signing failure never reaches it.
	req, _ := http.NewRequest(http.MethodGet, "/r/x", nil)
	resp, err := h.DoSigned(ctx, "acme_driver", req, Class{}, signWith("acme_driver.other", &keys))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a principal for another route = %d", resp.StatusCode)
	}
	broken := errors.New("no principal")
	_, err = h.DoSigned(ctx, "acme_driver", req, Class{}, func(extapi.Digest, []byte, http.Header) error { return broken })
	if !errors.Is(err, broken) {
		t.Fatalf("a signing failure = %v", err)
	}
}

// A stream call is held to Class.Timeout only until its headers: one that
// keeps writing runs past it, one that sends no headers in time is a
// timeout of a running plugin. A plain call is held to Class.Timeout even
// when ctx allows longer.
func TestClassTimeoutBoundsStreamHeadersAndPlainCalls(t *testing.T) {
	sp := &fakeSpawner{config: func(int) pluginsdk.Config {
		c := sdkConfig("acme_feed")
		c.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/r/feed":
				for range 6 {
					_, _ = io.WriteString(w, "tick\n")
					w.(http.Flusher).Flush()
					time.Sleep(40 * time.Millisecond)
				}
			case "/r/silent", "/r/slow":
				time.Sleep(time.Second)
			}
		})
		return c
	}}
	h, _ := newTestHost(t, sp, nil, fastTiming())
	_ = h.Start(testSpec(t, "acme_feed"))
	waitStatus(t, h, "acme_feed", inState(StateRunning))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := Class{Stream: true, Timeout: 100 * time.Millisecond}

	resp, err := callPlugin(ctx, h, "acme_feed", "/r/feed", stream)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || len(body) != len("tick\n")*6 {
		t.Fatalf("a stream writing past its timeout = %q, %v", body, err)
	}

	for _, tc := range []struct {
		path  string
		class Class
	}{{"/r/silent", stream}, {"/r/slow", Class{Timeout: 100 * time.Millisecond}}} {
		start := time.Now()
		_, err = callPlugin(ctx, h, "acme_feed", tc.path, tc.class)
		code, params := productParams(t, err)
		if code != "PLUGIN_TIMEOUT" || params["state"] != "running" || time.Since(start) > 700*time.Millisecond {
			t.Fatalf("%s = %s %v after %v", tc.path, code, params, time.Since(start))
		}
	}
}
