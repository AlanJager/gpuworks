package apiclient

import (
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"sync"
)

type RequestPhase string

const (
	PhaseConnect  RequestPhase = "connect"
	PhaseTLS      RequestPhase = "tls"
	PhaseResponse RequestPhase = "response"
)

func traceConnection(req *http.Request) (*http.Request, func() RequestPhase) {
	var mu sync.Mutex
	var phase RequestPhase
	set := func(value RequestPhase) {
		mu.Lock()
		defer mu.Unlock()
		// A losing parallel dial must not erase an established connection.
		if phase == PhaseResponse || (phase == PhaseTLS && value == PhaseConnect) {
			return
		}
		phase = value
	}
	trace := &httptrace.ClientTrace{
		DNSStart:          func(httptrace.DNSStartInfo) { set(PhaseConnect) },
		ConnectStart:      func(string, string) { set(PhaseConnect) },
		TLSHandshakeStart: func() { set(PhaseTLS) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil {
				set(PhaseResponse)
			}
		},
		GotConn: func(httptrace.GotConnInfo) { set(PhaseResponse) },
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace)), func() RequestPhase {
		mu.Lock()
		defer mu.Unlock()
		return phase
	}
}

func requestUsesProxy(client *http.Client, req *http.Request) bool {
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if tr, ok := transport.(*http.Transport); ok && tr.Proxy != nil {
		proxy, err := tr.Proxy(req)
		return err != nil || proxy != nil
	}
	return false
}
