package compshare

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// listingServer serves DescribeCompShareInstance over a fixed account the way
// a UCloud-style API pages: Offset/Limit select a window, the page size is
// capped, and TotalCount reports the account size. Hooks bend one behaviour.
type listingServer struct {
	mu            sync.Mutex
	total         int
	pageCap       int
	ignoreOffset  bool
	omitTotal     bool
	truncateAfter int // serve nothing past this many instances (0 = off)
	failOffset    int // RetCode error for this offset (-1 = off)
	calls         []string
}

func (s *listingServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("Action") != listAction {
			t.Errorf("Action = %q", r.Form.Get("Action"))
		}
		if r.Form.Get("Signature") == "" || r.Form.Get("PublicKey") != "pub" {
			t.Errorf("request is not signed: %v", r.Form)
		}
		offset, err := strconv.Atoi(r.Form.Get("Offset"))
		if err != nil {
			t.Errorf("Offset = %q", r.Form.Get("Offset"))
		}
		limit, err := strconv.Atoi(r.Form.Get("Limit"))
		if err != nil || limit < 1 || limit > 100 {
			t.Errorf("Limit = %q", r.Form.Get("Limit"))
		}
		s.mu.Lock()
		s.calls = append(s.calls, r.Form.Get("Offset")+"/"+r.Form.Get("Limit"))
		s.mu.Unlock()
		if offset == s.failOffset {
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 230, "Message": "Params [Offset] not available"})
			return
		}
		if s.ignoreOffset {
			offset = 0
		}
		if s.pageCap > 0 && limit > s.pageCap {
			limit = s.pageCap
		}
		available := s.total
		if s.truncateAfter > 0 && available > s.truncateAfter {
			available = s.truncateAfter
		}
		hosts := []map[string]any{}
		for i := offset; i < available && i < offset+limit; i++ {
			hosts = append(hosts, map[string]any{
				"UHostId": fmt.Sprintf("uhost-%03d", i), "Name": fmt.Sprintf("gpu-%d", i), "State": "Running",
				"Region": "cn-wlcb", "Zone": "cn-wlcb-01", "GPU": 1, "GpuType": "4090",
			})
		}
		body := map[string]any{"Action": listAction + "Response", "RetCode": 0, "UHostSet": hosts}
		if !s.omitTotal {
			body["TotalCount"] = s.total
		}
		_ = json.NewEncoder(w).Encode(body)
	}
}

func listWith(t *testing.T, s *listingServer) ([]provider.Instance, error) {
	t.Helper()
	srv := httptest.NewServer(s.handler(t))
	t.Cleanup(srv.Close)
	d := New()
	d.setBaseURL(srv.URL)
	return d.ListInstances(context.Background(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"})
}

func instanceIDs(insts []provider.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, inst := range insts {
		out = append(out, inst.ExternalID)
	}
	return out
}

func wantIDs(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("uhost-%03d", i))
	}
	return out
}

func TestListInstancesPagesThroughWholeAccount(t *testing.T) {
	for _, tc := range []struct {
		name      string
		server    *listingServer
		wantCalls []string
		wantCount int
	}{
		{name: "empty account", server: &listingServer{total: 0}, wantCalls: []string{"0/100"}},
		{name: "one page", server: &listingServer{total: 3}, wantCalls: []string{"0/100"}, wantCount: 3},
		{name: "exactly one full page", server: &listingServer{total: 100}, wantCalls: []string{"0/100"}, wantCount: 100},
		{name: "several pages", server: &listingServer{total: 230}, wantCalls: []string{"0/100", "100/100", "200/100"}, wantCount: 230},
		{name: "smaller page cap", server: &listingServer{total: 120, pageCap: 50}, wantCalls: []string{"0/100", "50/100", "100/100"}, wantCount: 120},
		{name: "no TotalCount", server: &listingServer{total: 103, omitTotal: true}, wantCalls: []string{"0/100", "100/100"}, wantCount: 103},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.server
			s.failOffset = -1
			got, err := listWith(t, s)
			if err != nil {
				t.Fatalf("ListInstances: %v", err)
			}
			if !reflect.DeepEqual(instanceIDs(got), wantIDs(tc.wantCount)) {
				t.Fatalf("ids = %v", instanceIDs(got))
			}
			s.mu.Lock()
			calls := append([]string(nil), s.calls...)
			s.mu.Unlock()
			if !reflect.DeepEqual(calls, tc.wantCalls) {
				t.Fatalf("calls = %v, want %v", calls, tc.wantCalls)
			}
		})
	}
}

// A short or inconsistent listing must fail: sync would otherwise remove the
// rows of instances that still exist.
func TestListInstancesRejectsIncompleteListing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		server  *listingServer
		wantErr string
	}{
		{name: "offset ignored", server: &listingServer{total: 150, ignoreOffset: true, failOffset: -1}, wantErr: "repeated uhost-000"},
		{name: "ends before TotalCount", server: &listingServer{total: 150, truncateAfter: 100, failOffset: -1}, wantErr: "ended at 100 of 150"},
		{name: "later page fails", server: &listingServer{total: 150, failOffset: 100}, wantErr: "RetCode 230"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.server
			got, err := listWith(t, s)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ListInstances = %d instances, err=%v; want error containing %q", len(got), err, tc.wantErr)
			}
			if got != nil {
				t.Fatalf("partial listing returned alongside error: %d instances", len(got))
			}
		})
	}
}
