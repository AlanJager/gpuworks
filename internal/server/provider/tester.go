package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider/testconn"
)

// ConnectionTester is an optional driver extension: vendor-specific checks
// (auth, protocol capability, read permissions) run after the generic network
// layer. creds must never appear in check details or logs.
type ConnectionTester interface {
	ConnectionChecks(creds Credentials) []testconn.VendorCheck
}

// requiredCredentialNames resolves the driver's own credential schema when it
// implements CredentialSchemaProvider, falling back to the static kind table.
func requiredCredentialNames(drv Driver) []string {
	var names []string
	for _, f := range driverCredentialFields(drv) {
		if f.Required {
			names = append(names, f.Name)
		}
	}
	return names
}

func credentialSatisfied(creds Credentials) func(string) bool {
	return func(name string) bool {
		switch name {
		case "endpoint":
			return strings.TrimSpace(creds.Endpoint) != ""
		case "api_key":
			return strings.TrimSpace(creds.APIKey) != ""
		case "public_key":
			return strings.TrimSpace(creds.PublicKey) != ""
		case "private_key":
			return strings.TrimSpace(creds.PrivateKey) != ""
		default:
			return strings.TrimSpace(creds.Extra[name]) != ""
		}
	}
}

// Check, Summary and ReportFunc are re-exported so the api layer consumes
// connection-test results through this package instead of importing testconn
// directly (api_fanout budget; see scripts/arch-constraints.md).
type (
	Check      = testconn.Check
	Summary    = testconn.Summary
	ReportFunc = testconn.ReportFunc
)

// StatusFailed is re-exported alongside the type aliases above.
const StatusFailed = testconn.StatusFailed

// connectionAuthTimeout bounds the generic credential check; the drivers'
// own clients time out sooner.
const connectionAuthTimeout = 30 * time.Second

// SetConnectionTestRunner replaces the runner connection tests use, before
// requests are served. A server keeps the real one (nil); a harness answers
// the DNS, TCP and TLS steps itself instead of reaching the network.
func (s *Service) SetConnectionTestRunner(r *testconn.Runner) {
	s.connectionTests = r
}

func (s *Service) connectionTestRunner() *testconn.Runner {
	if s.connectionTests != nil {
		return s.connectionTests
	}
	return testconn.New()
}

// hasEndpointField reports whether the kind's credential schema takes an
// endpoint at all.
func hasEndpointField(drv Driver) bool {
	for _, f := range driverCredentialFields(drv) {
		if f.Name == "endpoint" && !f.Extra {
			return true
		}
	}
	return false
}

// credentialRedactor replaces every credential value except the endpoint in a
// text bound for the server log. The endpoint is what the log line is about
// and the provider's own read route returns it; everything else is treated as
// a secret, declared one or not.
func credentialRedactor(creds Credentials) func(string) string {
	values := []string{creds.APIKey, creds.PublicKey, creds.PrivateKey}
	for _, v := range creds.Extra {
		values = append(values, v)
	}
	// Longest first, so a value that contains another is replaced whole.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	var pairs []string
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			pairs = append(pairs, v, "[redacted]")
		}
	}
	replacer := strings.NewReplacer(pairs...)
	return replacer.Replace
}

// credentialAuthCheck is the vendor check of a driver that brings none of its
// own: the driver's cheap credential validation call. The driver's error text
// may quote the vendor's reply, so only the code reaches the wire.
func credentialAuthCheck(drv Driver, creds Credentials) testconn.VendorCheck {
	return testconn.VendorCheck{
		Name:    testconn.CheckAuthRequest,
		Depends: []string{testconn.CheckCredentialsShape, testconn.CheckTCPConnect},
		Run: func(ctx context.Context) (map[string]any, error) {
			ctx, cancel := context.WithTimeout(ctx, connectionAuthTimeout)
			defer cancel()
			if err := drv.ValidateCredentials(ctx, creds); err != nil {
				return nil, &testconn.Failure{Code: messages.ErrorProviderAuthFailed, Hint: testconn.HintAuth, Err: err}
			}
			return nil, nil
		},
	}
}

// TestConnection runs the layered connection test for explicit credentials.
// An empty endpoint is the kind's default one, as everywhere else; a kind
// that has none and takes none (its driver picks them) skips the endpoint and
// network checks and goes on to the vendor checks.
func (s *Service) TestConnection(ctx context.Context, kind string, creds Credentials, report testconn.ReportFunc) (testconn.Summary, error) {
	drv, ok := s.driver(kind)
	if !ok {
		return testconn.Summary{}, fmt.Errorf("unknown provider kind: %s", kind)
	}
	endpoint := creds.EndpointOr(defaultEndpoint(kind))
	in := testconn.Input{
		Kind:                  kind,
		Endpoint:              endpoint,
		EndpointNotApplicable: endpoint == "" && !hasEndpointField(drv),
		RequiredCredentials:   requiredCredentialNames(drv),
		CredentialsSatisfied:  credentialSatisfied(creds),
		Redact:                credentialRedactor(creds),
	}
	if tester, ok := drv.(ConnectionTester); ok {
		in.Vendor = tester.ConnectionChecks(creds)
	} else {
		in.Vendor = []testconn.VendorCheck{credentialAuthCheck(drv, creds)}
	}
	return s.connectionTestRunner().Run(ctx, in, report)
}

// TestSavedConnection tests a stored provider. A non-empty field of overrides
// replaces the stored one for this test only, merged as Update merges an
// edit, so a blank secret means the stored value.
func (s *Service) TestSavedConnection(ctx context.Context, providerID uint, overrides Credentials, report testconn.ReportFunc) (testconn.Summary, error) {
	p, err := s.store.GetProvider(providerID)
	if err != nil {
		return testconn.Summary{}, err
	}
	creds, err := s.creds(p)
	if err != nil {
		return testconn.Summary{}, err
	}
	return s.TestConnection(ctx, p.Kind, mergeCredentials(creds, overrides), report)
}
