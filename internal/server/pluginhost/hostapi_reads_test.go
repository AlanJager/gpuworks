package pluginhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// fakeReads answers the reads from a fixed fleet of node ids 1..n and
// records every call, so a test sees exactly what each request asked of
// the store.
type fakeReads struct {
	mu    sync.Mutex
	n     uint64
	calls []readCall
	err   error
	// broken makes answers break the wire rules.
	broken bool
}

type readCall struct {
	what   string
	ids    []uint64
	key    []byte
	cursor uint64
	limit  int
}

func (f *fakeReads) NodeIdentity(_ context.Context, ids []uint64, key []byte) ([]extapi.NodeIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, readCall{what: "identity", ids: append([]uint64(nil), ids...), key: key})
	if f.err != nil {
		return nil, f.err
	}
	var out []extapi.NodeIdentity
	for _, id := range slices.Sorted(slices.Values(ids)) {
		if id <= f.n {
			n := extapi.NodeIdentity{NodeID: id, Cards: []extapi.IdentityPart{}}
			if f.broken {
				n.Cards = nil
			}
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeReads) NodeInventory(_ context.Context, cursor uint64, limit int) ([]extapi.InventoryNode, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, readCall{what: "inventory", cursor: cursor, limit: limit})
	if f.err != nil {
		return nil, 0, f.err
	}
	var out []extapi.InventoryNode
	for id := cursor + 1; id <= f.n && len(out) < limit; id++ {
		out = append(out, extapi.InventoryNode{ID: id, UUID: fmt.Sprintf("node-%d", id), Name: fmt.Sprintf("gpu-%d", id),
			EnrolledAt: extapi.NewTime(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)), Cards: []extapi.InventoryCard{}})
	}
	var next uint64
	if len(out) == limit && out[len(out)-1].ID < f.n {
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

func (f *fakeReads) recorded() []readCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]readCall(nil), f.calls...)
}

var readPermissions = []string{"read:node_identity", "read:node_inventory"}

func readerOwner(lifecycle string, perms ...string) Owner {
	return Owner{ID: "acme_backup", Version: "1.0.0", Lifecycle: lifecycle,
		Grant: extapi.PluginGrant{ID: "acme_backup", Version: "1.0.0", PermissionsGranted: perms},
		Use:   map[string]int{extapi.CapHostNodeIdentity: 1, extapi.CapHostNodes: 1}}
}

func readerConn(t *testing.T, reads Reads, o Owner) *hostConn {
	t.Helper()
	spec := testSpec(t, "acme_backup")
	spec.Lifecycle = o.Lifecycle
	h := newHostConn(&spec, nil, reads, slog.New(slog.DiscardHandler))
	h.accept(o)
	return h
}

type readAnswer struct {
	status int
	wire   *extapi.WireError
	body   []byte
}

func readHost(t *testing.T, h *hostConn, method, target, body string) readAnswer {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader(body)))
	a := readAnswer{status: rec.Code, body: rec.Body.Bytes()}
	if rec.Code/100 != 2 {
		w, err := extapi.DecodeWireError(a.body)
		if err != nil {
			t.Fatalf("%s %s: %d with %s: %v", method, target, rec.Code, a.body, err)
		}
		a.wire = w
	}
	return a
}

func wantRefusal(t *testing.T, a readAnswer, status int, code string, params extapi.Params) {
	t.Helper()
	if a.status != status || a.wire == nil || a.wire.Code != code {
		t.Fatalf("answer %d %+v, want %d %s", a.status, a.wire, status, code)
	}
	for k, v := range params {
		if fmt.Sprint(a.wire.Params[k]) != fmt.Sprint(v) {
			t.Fatalf("%s params %v, want %s=%v", code, a.wire.Params, k, v)
		}
	}
}

const identityBody = `{"node_ids":[2,9,1],"salt":"acme-2026"}`

// A granted, negotiated read answers with the wire objects, from one call
// of the reads per request: the named nodes that exist, keyed with the
// salt bound to the package, and one page of the inventory.
func TestHostAPIReadsAnswerFromOnePinnedRead(t *testing.T) {
	reads := &fakeReads{n: 5}
	h := readerConn(t, reads, readerOwner(extapi.LifecycleResident, readPermissions...))

	a := readHost(t, h, http.MethodPost, extapi.PathHostNodeIdentity, identityBody)
	if a.status != 200 {
		t.Fatalf("identity = %d %+v", a.status, a.wire)
	}
	ans, err := extapi.DecodeNodeIdentityAnswer(a.body)
	if err != nil || len(ans.Nodes) != 2 || ans.Nodes[0].NodeID != 1 || ans.Nodes[1].NodeID != 2 {
		t.Fatalf("identity answer = %s, %v", a.body, err)
	}
	// Nothing exists: an empty list, never null.
	if a := readHost(t, h, http.MethodPost, extapi.PathHostNodeIdentity, `{"node_ids":[99],"salt":"s"}`); string(a.body) != `{"nodes":[]}` {
		t.Fatalf("identity of missing nodes = %d %s", a.status, a.body)
	}

	a = readHost(t, h, http.MethodGet, extapi.PathHostNodes+"?limit=2", "")
	page, err := extapi.DecodeNodesPage(a.body)
	if a.status != 200 || err != nil || len(page.Nodes) != 2 || page.Next != "2" {
		t.Fatalf("first page = %d %s, %v", a.status, a.body, err)
	}
	a = readHost(t, h, http.MethodGet, extapi.PathHostNodes+"?cursor="+page.Next+"&limit=3", "")
	page, err = extapi.DecodeNodesPage(a.body)
	if err != nil || len(page.Nodes) != 3 || page.Nodes[0].ID != 3 || page.Next != "" {
		t.Fatalf("last page = %s, %v", a.body, err)
	}
	readHost(t, h, http.MethodGet, extapi.PathHostNodes, "")

	calls := reads.recorded()
	want := []readCall{
		{what: "identity", ids: []uint64{2, 9, 1}, key: IdentityKey("acme_backup", "acme-2026")},
		{what: "identity", ids: []uint64{99}, key: IdentityKey("acme_backup", "s")},
		{what: "inventory", cursor: 0, limit: 2},
		{what: "inventory", cursor: 2, limit: 3},
		{what: "inventory", cursor: 0, limit: extapi.MaxNodeRead},
	}
	if len(calls) != len(want) {
		t.Fatalf("reads made %d calls, want one per request: %+v", len(calls), calls)
	}
	for i, c := range calls {
		w := want[i]
		if c.what != w.what || fmt.Sprint(c.ids) != fmt.Sprint(w.ids) || !bytes.Equal(c.key, w.key) || c.cursor != w.cursor || c.limit != w.limit {
			t.Errorf("call %d = %+v, want %+v", i, c, w)
		}
	}
}

// The digest key binds the salt to the package: the same salt from two
// packages is two keys; the same package and salt is one key everywhere.
// The longest package id with the longest salt fits the store's 256 bytes.
func TestIdentityKeyIsBoundToThePackage(t *testing.T) {
	if bytes.Equal(IdentityKey("acme_a", "s"), IdentityKey("acme_b", "s")) {
		t.Fatal("two packages share a key")
	}
	if !bytes.Equal(IdentityKey("acme_a", "s"), IdentityKey("acme_a", "s")) {
		t.Fatal("one package's key changes")
	}
	// Without the separators "acme_a"+"bc" and "acme_ab"+"c" would collide.
	if bytes.Equal(IdentityKey("acme_a", "bc"), IdentityKey("acme_ab", "c")) {
		t.Fatal("the key is ambiguous")
	}
	if n := len(IdentityKey("a"+strings.Repeat("b", 31), strings.Repeat("s", extapi.MaxNodeIdentitySalt))); n != 256 {
		t.Fatalf("longest key = %d bytes", n)
	}
}

// Each read needs its permission and its negotiated capability; an
// on-demand plugin may read, since reads are not pushes.
func TestHostAPIReadsNeedPermissionAndCapability(t *testing.T) {
	reads := &fakeReads{n: 3}
	none := readerConn(t, reads, readerOwner(extapi.LifecycleResident))
	wantRefusal(t, readHost(t, none, http.MethodPost, extapi.PathHostNodeIdentity, identityBody), 403,
		messages.ErrorPluginHostNotPermitted, extapi.Params{"id": "acme_backup", "permission": "read:node_identity"})
	wantRefusal(t, readHost(t, none, http.MethodGet, extapi.PathHostNodes, ""), 403,
		messages.ErrorPluginHostNotPermitted, extapi.Params{"id": "acme_backup", "permission": "read:node_inventory"})

	o := readerOwner(extapi.LifecycleResident, readPermissions...)
	o.Use = map[string]int{"table.banners": 1}
	unnegotiated := readerConn(t, reads, o)
	wantRefusal(t, readHost(t, unnegotiated, http.MethodPost, extapi.PathHostNodeIdentity, identityBody), 412,
		messages.ErrorPluginCapabilityNotOffered, extapi.Params{"id": "acme_backup", "capability": "host.node_identity", "version": 1})
	wantRefusal(t, readHost(t, unnegotiated, http.MethodGet, extapi.PathHostNodes, ""), 412,
		messages.ErrorPluginCapabilityNotOffered, extapi.Params{"id": "acme_backup", "capability": "host.nodes", "version": 1})
	if n := len(reads.recorded()); n != 0 {
		t.Fatalf("refused reads reached the store %d times", n)
	}

	onDemand := readerConn(t, reads, readerOwner(extapi.LifecycleOnDemand, readPermissions...))
	if a := readHost(t, onDemand, http.MethodPost, extapi.PathHostNodeIdentity, identityBody); a.status != 200 {
		t.Fatalf("on-demand identity = %d %+v", a.status, a.wire)
	}
	if a := readHost(t, onDemand, http.MethodGet, extapi.PathHostNodes, ""); a.status != 200 {
		t.Fatalf("on-demand inventory = %d %+v", a.status, a.wire)
	}
}

// Arguments out of bounds are refused with the field before any read.
func TestHostAPIReadsRefuseInvalidArguments(t *testing.T) {
	reads := &fakeReads{n: 3}
	h := readerConn(t, reads, readerOwner(extapi.LifecycleResident, readPermissions...))
	ids := make([]string, extapi.MaxNodeRead+1)
	for i := range ids {
		ids[i] = fmt.Sprint(i + 1)
	}
	bodies := map[string]string{
		`{"node_ids":[1],"salt":""}`: "salt",
		`{"node_ids":[1],"salt":"` + strings.Repeat("s", extapi.MaxNodeIdentitySalt+1) + `"}`: "salt",
		`{"node_ids":[],"salt":"s"}`:                               "node_ids",
		`{"node_ids":[` + strings.Join(ids, ",") + `],"salt":"s"}`: "node_ids",
		`{"node_ids":[0],"salt":"s"}`:                              "node_ids.0",
		`{"node_ids":[4,4],"salt":"s"}`:                            "node_ids.1",
	}
	for body, field := range bodies {
		wantRefusal(t, readHost(t, h, http.MethodPost, extapi.PathHostNodeIdentity, body), 400,
			messages.ErrorPluginHostArgumentInvalid, extapi.Params{"id": "acme_backup", "field": field})
	}
	// A body that is not the object keeps its wire code.
	wantRefusal(t, readHost(t, h, http.MethodPost, extapi.PathHostNodeIdentity, `{"node_ids":[1],"salt":"s","x":1}`), 400,
		extapi.ErrorWireUnknownField, extapi.Params{"field": "x"})
	wantRefusal(t, readHost(t, h, http.MethodPost, extapi.PathHostNodeIdentity, `[`), 400, extapi.ErrorWireMalformed, nil)
	// A body over the bound is too large, however far over it is.
	for _, n := range []int{extapi.MaxNodeIdentityRequestBytes, 1 << 20} {
		huge := `{"node_ids":[1],"salt":"` + strings.Repeat("s", n) + `"}`
		wantRefusal(t, readHost(t, h, http.MethodPost, extapi.PathHostNodeIdentity, huge), 413,
			messages.ErrorRequestTooLarge, extapi.Params{"max_bytes": extapi.MaxNodeIdentityRequestBytes})
	}

	// Refused queries cost no page: a bucket with one page left still
	// serves a valid page after them all.
	h.pages = newTokenBucket(1, 1, time.Now)
	queries := map[string]string{
		"limit=0": "limit", "limit=501": "limit", "limit=x": "limit",
		"cursor=abc": "cursor", "cursor=0": "cursor", "cursor=07": "cursor", "cursor=a/b": "cursor",
	}
	for q, field := range queries {
		wantRefusal(t, readHost(t, h, http.MethodGet, extapi.PathHostNodes+"?"+q, ""), 400,
			messages.ErrorPluginHostArgumentInvalid, extapi.Params{"id": "acme_backup", "field": field})
	}
	if n := len(reads.recorded()); n != 0 {
		t.Fatalf("invalid reads reached the store %d times", n)
	}
	if a := readHost(t, h, http.MethodGet, extapi.PathHostNodes, ""); a.status != 200 {
		t.Fatalf("a valid page after refused queries = %d %+v", a.status, a.wire)
	}
}

// Inventory pages have their own bucket of 2 a second with a burst of 4,
// on top of the host API's: once the burst is spent, the third page within
// a second is refused and tells the plugin when to retry.
func TestHostAPIInventoryPageBucket(t *testing.T) {
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	reads := &fakeReads{n: 3}
	h := readerConn(t, reads, readerOwner(extapi.LifecycleResident, readPermissions...))
	h.pages = newTokenBucket(hostPagesPerSecond, hostPagesBurst, clock)
	page := func() readAnswer { return readHost(t, h, http.MethodGet, extapi.PathHostNodes, "") }
	for i := range hostPagesBurst {
		if a := page(); a.status != 200 {
			t.Fatalf("burst page %d = %d %+v", i, a.status, a.wire)
		}
	}
	now = now.Add(time.Second)
	for i := range 2 {
		if a := page(); a.status != 200 {
			t.Fatalf("page %d of the second = %d %+v", i, a.status, a.wire)
		}
	}
	a := page()
	wantRefusal(t, a, 429, messages.ErrorPluginHostRateLimited, extapi.Params{"limit": hostPagesPerSecond})
	if a.wire.RetryAfterMS != 500 {
		t.Fatalf("retry after %d ms", a.wire.RetryAfterMS)
	}
	// Identity reads do not draw on the page bucket.
	if a := readHost(t, h, http.MethodPost, extapi.PathHostNodeIdentity, identityBody); a.status != 200 {
		t.Fatalf("identity after the page bucket ran out = %d %+v", a.status, a.wire)
	}
	if n := len(reads.recorded()); n != hostPagesBurst+2+1 {
		t.Fatalf("reads made %d calls", n)
	}
}

// A server without reads, a wrong method, a failing store and an answer
// that breaks the wire rules each answer a coded refusal.
func TestHostAPIReadsRefusals(t *testing.T) {
	without := readerConn(t, nil, readerOwner(extapi.LifecycleResident, readPermissions...))
	wantRefusal(t, readHost(t, without, http.MethodPost, extapi.PathHostNodeIdentity, identityBody), 404,
		messages.ErrorPluginHostRouteUnknown, extapi.Params{"path": extapi.PathHostNodeIdentity})
	wantRefusal(t, readHost(t, without, http.MethodGet, extapi.PathHostNodes, ""), 404,
		messages.ErrorPluginHostRouteUnknown, extapi.Params{"path": extapi.PathHostNodes})

	reads := &fakeReads{n: 3}
	h := readerConn(t, reads, readerOwner(extapi.LifecycleResident, readPermissions...))
	wantRefusal(t, readHost(t, h, http.MethodGet, extapi.PathHostNodeIdentity, ""), 501,
		messages.ErrorPluginHostRouteUnknown, extapi.Params{"method": "GET"})
	wantRefusal(t, readHost(t, h, http.MethodPost, extapi.PathHostNodes, ""), 501,
		messages.ErrorPluginHostRouteUnknown, extapi.Params{"method": "POST"})

	reads.err = errors.New("database is locked")
	wantRefusal(t, readHost(t, h, http.MethodPost, extapi.PathHostNodeIdentity, identityBody), 500,
		messages.ErrorPluginHostFailed, extapi.Params{"detail": "database is locked"})
	wantRefusal(t, readHost(t, h, http.MethodGet, extapi.PathHostNodes, ""), 500,
		messages.ErrorPluginHostFailed, extapi.Params{"detail": "database is locked"})
	reads.err = Refuse(extapi.ReasonUnavailable, messages.ErrorPluginUnavailable, "closing", map[string]any{"plugin": "core"})
	wantRefusal(t, readHost(t, h, http.MethodPost, extapi.PathHostNodeIdentity, identityBody), 503,
		messages.ErrorPluginUnavailable, nil)

	reads.err, reads.broken = nil, true
	wantRefusal(t, readHost(t, h, http.MethodPost, extapi.PathHostNodeIdentity, identityBody), 500,
		messages.ErrorPluginHostFailed, nil)

	// A refused handshake refuses reads like pushes.
	spec := testSpec(t, "acme_backup")
	refused := newHostConn(&spec, nil, reads, slog.New(slog.DiscardHandler))
	refused.reject()
	wantRefusal(t, readHost(t, refused, http.MethodGet, extapi.PathHostNodes, ""), 412, messages.ErrorPluginHostNotReady, nil)
}
