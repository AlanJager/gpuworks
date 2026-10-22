package conn

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestLocalAddrClientRemembersTheDialedSocket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	client, dialedFrom := localAddrClient()
	if dialedFrom() != nil {
		t.Fatal("an address before any dial")
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	tcp, ok := dialedFrom().(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() || tcp.Port == 0 {
		t.Fatalf("local = %v", dialedFrom())
	}
}

func TestManagementComesFromTheConnection(t *testing.T) {
	c := &Conn{creds: Credentials{ServerURL: "http://172.25.17.156:8800"}}
	inv := proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "eth1", Addrs: []string{"10.100.10.12/24"}}}}
	c.setLocal(&net.TCPAddr{IP: net.ParseIP("10.100.10.12"), Port: 40000}, false)
	if got := c.management(inv); got == nil || got.IP != "10.100.10.12" || got.Interface != "eth1" {
		t.Fatalf("management = %+v", got)
	}
	// A server_dial connection accepted on a websocket-less address type
	// carries no IP; the default route stands in rather than nothing.
	c.setLocal(nil, false)
	if got := c.management(inv); got != nil && net.ParseIP(got.IP) == nil {
		t.Fatalf("fallback = %+v", got)
	}
}
