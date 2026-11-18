package pluginhost

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

const hostIdentityBody = `{"salt":"acme-2026"}`

// A granted, negotiated host-identity read answers the wire object from
// one call of the reads, keyed with the salt bound to the package exactly
// as the node identity is.
func TestHostAPIHostIdentityAnswersFromOneRead(t *testing.T) {
	reads := &fakeReads{n: 1}
	h := readerConn(t, reads, readerOwner(extapi.LifecycleResident, readPermissions...))
	a := readHost(t, h, http.MethodPost, extapi.PathHostHostIdentity, hostIdentityBody)
	if a.status != 200 || string(a.body) != `{"system_uuid":{"h":"5f0c8e2a9d413b77","tail":"3432"}}` {
		t.Fatalf("host identity = %d %s", a.status, a.body)
	}
	if _, err := extapi.DecodeHostIdentityAnswer(a.body); err != nil {
		t.Fatal(err)
	}
	readHost(t, h, http.MethodPost, extapi.PathHostNodeIdentity, `{"node_ids":[1],"salt":"acme-2026"}`)
	calls := reads.recorded()
	if len(calls) != 2 || calls[0].what != "host" || !bytes.Equal(calls[0].key, IdentityKey("acme_backup", "acme-2026")) ||
		!bytes.Equal(calls[0].key, calls[1].key) {
		t.Fatalf("calls = %+v", calls)
	}
	onDemand := readerConn(t, reads, readerOwner(extapi.LifecycleOnDemand, readPermissions...))
	if a := readHost(t, onDemand, http.MethodPost, extapi.PathHostHostIdentity, hostIdentityBody); a.status != 200 {
		t.Fatalf("on-demand host identity = %d %+v", a.status, a.wire)
	}
}

// The read needs read:host_identity and host.host_identity; the node
// permissions are not enough. A refused read never reaches the reads.
func TestHostAPIHostIdentityNeedsItsPermissionAndCapability(t *testing.T) {
	reads := &fakeReads{n: 1}
	nodeOnly := readerConn(t, reads, readerOwner(extapi.LifecycleResident, "read:node_identity", "read:node_inventory"))
	wantRefusal(t, readHost(t, nodeOnly, http.MethodPost, extapi.PathHostHostIdentity, hostIdentityBody), 403,
		messages.ErrorPluginHostNotPermitted, extapi.Params{"id": "acme_backup", "permission": "read:host_identity"})
	o := readerOwner(extapi.LifecycleResident, readPermissions...)
	o.Use = map[string]int{extapi.CapHostNodeIdentity: 1}
	unnegotiated := readerConn(t, reads, o)
	wantRefusal(t, readHost(t, unnegotiated, http.MethodPost, extapi.PathHostHostIdentity, hostIdentityBody), 412,
		messages.ErrorPluginCapabilityNotOffered, extapi.Params{"id": "acme_backup", "capability": "host.host_identity", "version": 1})
	if n := len(reads.recorded()); n != 0 {
		t.Fatalf("refused reads reached the reads %d times", n)
	}
}

// A bad salt is the plugin's argument error, a body that is not the
// object keeps its wire code, and a body over 4 KiB is too large; none
// reaches the reads. A wrong method, a server without reads, a failing
// read and an answer that breaks the wire rules each answer a coded
// refusal.
func TestHostAPIHostIdentityRefusals(t *testing.T) {
	reads := &fakeReads{n: 1}
	h := readerConn(t, reads, readerOwner(extapi.LifecycleResident, readPermissions...))
	for _, body := range []string{`{"salt":""}`, `{"salt":"` + strings.Repeat("s", extapi.MaxNodeIdentitySalt+1) + `"}`} {
		wantRefusal(t, readHost(t, h, http.MethodPost, extapi.PathHostHostIdentity, body), 400,
			messages.ErrorPluginHostArgumentInvalid, extapi.Params{"id": "acme_backup", "field": "salt"})
	}
	wantRefusal(t, readHost(t, h, http.MethodPost, extapi.PathHostHostIdentity, `{"node_ids":[1],"salt":"s"}`), 400,
		extapi.ErrorWireUnknownField, extapi.Params{"field": "node_ids"})
	wantRefusal(t, readHost(t, h, http.MethodPost, extapi.PathHostHostIdentity, `{`), 400, extapi.ErrorWireMalformed, nil)
	huge := `{"salt":"` + strings.Repeat("s", extapi.MaxHostIdentityRequestBytes) + `"}`
	wantRefusal(t, readHost(t, h, http.MethodPost, extapi.PathHostHostIdentity, huge), 413,
		messages.ErrorRequestTooLarge, extapi.Params{"max": extapi.MaxHostIdentityRequestBytes})
	if n := len(reads.recorded()); n != 0 {
		t.Fatalf("invalid reads reached the reads %d times", n)
	}
	wantRefusal(t, readHost(t, h, http.MethodGet, extapi.PathHostHostIdentity, ""), 501,
		messages.ErrorPluginHostRouteUnknown, extapi.Params{"method": "GET"})

	without := readerConn(t, nil, readerOwner(extapi.LifecycleResident, readPermissions...))
	wantRefusal(t, readHost(t, without, http.MethodPost, extapi.PathHostHostIdentity, hostIdentityBody), 404,
		messages.ErrorPluginHostRouteUnknown, extapi.Params{"path": extapi.PathHostHostIdentity})

	reads.err = errors.New("input/output error")
	wantRefusal(t, readHost(t, h, http.MethodPost, extapi.PathHostHostIdentity, hostIdentityBody), 500,
		messages.ErrorPluginHostFailed, extapi.Params{"detail": "input/output error"})
	reads.err, reads.broken = nil, true
	wantRefusal(t, readHost(t, h, http.MethodPost, extapi.PathHostHostIdentity, hostIdentityBody), 500,
		messages.ErrorPluginHostFailed, nil)
}
