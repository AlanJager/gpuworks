package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestBenchmarkReportLinksSurviveOpeningLocalExport(t *testing.T) {
	st, signer, _ := newSessionRouter(t)
	run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{Name: "offline export", State: "done"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, AuthSigner: signer}, auth.SessionPolicy{Proxies: listedProxies("127.0.0.1")})
	localFile, _ := url.Parse("file:///tmp/benchmark.html")
	for _, tc := range []struct {
		name, origin, peer, forwardedHost, forwardedProto, forwardedPort, want string
	}{
		{name: "HTTP port", origin: "http://reports.example:8800", peer: "198.51.100.7:1234", want: "http://reports.example:8800"},
		{name: "native HTTPS", origin: "https://reports.example:9443", peer: "198.51.100.7:1234", want: "https://reports.example:9443"},
		{name: "IPv6", origin: "http://[2001:db8::1]:8800", peer: "198.51.100.7:1234", want: "http://[2001:db8::1]:8800"},
		{name: "trusted proxy", origin: "http://internal.example:8800", peer: "127.0.0.1:1234", forwardedHost: "reports.example", forwardedProto: "https", forwardedPort: "8443", want: "https://reports.example:8443"},
		{name: "untrusted proxy", origin: "http://reports.example:8800", peer: "198.51.100.7:1234", forwardedHost: "spoofed.example", forwardedProto: "https", forwardedPort: "8443", want: "http://reports.example:8800"},
	} {
		for _, format := range []string{"html", "markdown"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/benchmarks/%d/report?format=%s", tc.origin, run.ID, format), nil)
				req.RemoteAddr = tc.peer
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("X-Forwarded-Host", tc.forwardedHost)
				req.Header.Set("X-Forwarded-Proto", tc.forwardedProto)
				req.Header.Set("X-Forwarded-Port", tc.forwardedPort)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("report: status=%d body=%s", rec.Code, rec.Body.String())
				}
				want := fmt.Sprintf("%s/evaluations/%d", tc.want, run.ID)
				marker, end := `href="`, `"`
				if format == "markdown" {
					marker, end = "**Evaluation:** <", ">"
				}
				_, tail, ok := strings.Cut(rec.Body.String(), marker)
				link, _, closed := strings.Cut(tail, end)
				if !ok || !closed || link != want {
					t.Fatalf("evaluation link=%q, want %q", link, want)
				}
				target, err := url.Parse(link)
				if err != nil || !target.IsAbs() || localFile.ResolveReference(target).String() != want {
					t.Fatalf("local export link does not resolve to server: %q, err=%v", link, err)
				}
			})
		}
	}
}
