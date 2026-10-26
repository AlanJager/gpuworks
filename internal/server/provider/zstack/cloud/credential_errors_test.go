package cloud

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
)

func validationErrorCode(t *testing.T, err error) string {
	t.Helper()
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Error() == "" {
		t.Fatalf("missing product error: %v", err)
	}
	return product.Code()
}

func TestCloudCredentialFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		api       apiclient.APIError
		cause     error
		diagnosis *endpointDiagnosis
		want      string
		diagnose  bool
	}{
		{"IP unreachable", apiclient.APIError{Phase: apiclient.PhaseConnect}, syscall.EHOSTUNREACH, nil, "ZSTACK_IP_UNREACHABLE", false},
		{"network unreachable", apiclient.APIError{Phase: apiclient.PhaseConnect}, syscall.ENETUNREACH, nil, "ZSTACK_IP_UNREACHABLE", false},
		{"port refused", apiclient.APIError{Phase: apiclient.PhaseConnect}, syscall.ECONNREFUSED, nil, "ZSTACK_PORT_REFUSED", false},
		{"DNS failed", apiclient.APIError{Phase: apiclient.PhaseConnect}, &net.DNSError{IsNotFound: true}, nil, "ZSTACK_DNS_FAILED", false},
		{"response timeout", apiclient.APIError{Phase: apiclient.PhaseResponse, Kind: apiclient.ErrorTimeout}, context.DeadlineExceeded, nil, "ZSTACK_RESPONSE_TIMEOUT", false},
		{"TLS failure", apiclient.APIError{Phase: apiclient.PhaseTLS}, nil, nil, "ZSTACK_TLS_FAILED", false},
		{"proxy refused", apiclient.APIError{Phase: apiclient.PhaseConnect, ViaProxy: true}, syscall.ECONNREFUSED, nil, "ZSTACK_PROXY_CONNECTION_FAILED", false},
		{"AK missing", apiclient.APIError{Kind: apiclient.ErrorAuthentication, AuthReason: apiclient.AuthAccessKeyInvalid}, nil, nil, "ZSTACK_ACCESS_KEY_INVALID", false},
		{"SK mismatch", apiclient.APIError{Kind: apiclient.ErrorAuthentication, AuthReason: apiclient.AuthSignatureInvalid}, nil, nil, "ZSTACK_SIGNATURE_INVALID", false},
		{"license", apiclient.APIError{Kind: apiclient.ErrorAuthentication, AuthReason: apiclient.AuthLicenseRequired}, nil, nil, "ZSTACK_LICENSE_REQUIRED", false},
		{"clock", apiclient.APIError{Kind: apiclient.ErrorAuthentication, AuthReason: apiclient.AuthClockSkew}, nil, nil, "ZSTACK_CLOCK_SKEW", false},
		{"unknown 403", apiclient.APIError{Kind: apiclient.ErrorAuthentication, StatusCode: 403}, nil, nil, "ZSTACK_AUTH_REJECTED", false},
		{"HTTP gateway failure", apiclient.APIError{Kind: apiclient.ErrorNetwork, StatusCode: 503}, nil, nil, "ZSTACK_REQUEST_FAILED", false},
		{"unknown phase timeout", apiclient.APIError{Kind: apiclient.ErrorTimeout}, nil, nil, "ZSTACK_REQUEST_TIMEOUT", false},
		{"TCP recovered", apiclient.APIError{Phase: apiclient.PhaseConnect}, context.DeadlineExceeded, &endpointDiagnosis{ip: ipNoReply}, "ZSTACK_CONNECTION_RECOVERED", true},
		{"IP responds TCP times out", apiclient.APIError{Phase: apiclient.PhaseConnect}, context.DeadlineExceeded, &endpointDiagnosis{tcpError: context.DeadlineExceeded, ip: ipResponded}, "ZSTACK_PORT_UNREACHABLE", true},
		{"both probes time out", apiclient.APIError{Phase: apiclient.PhaseConnect}, context.DeadlineExceeded, &endpointDiagnosis{tcpError: context.DeadlineExceeded, ip: ipNoReply}, "ZSTACK_NETWORK_UNCONFIRMED", true},
		{"IP probe unavailable", apiclient.APIError{Phase: apiclient.PhaseConnect}, context.DeadlineExceeded, &endpointDiagnosis{tcpError: context.DeadlineExceeded, ip: ipProbeUnavailable}, "ZSTACK_NETWORK_DIAGNOSTIC_UNAVAILABLE", true},
		{"TCP refusal beats no ping reply", apiclient.APIError{Phase: apiclient.PhaseConnect}, context.DeadlineExceeded, &endpointDiagnosis{tcpError: syscall.ECONNREFUSED, ip: ipNoReply}, "ZSTACK_PORT_REFUSED", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("credential validation failed: %w", errors.Join(&tc.api, tc.cause))
			if needsEndpointDiagnosis(err) != tc.diagnose {
				t.Fatal("incorrect decision to run probes")
			}
			converted := credentialError(err, tc.diagnosis)
			if code := validationErrorCode(t, converted); code != tc.want {
				t.Fatalf("got %s, want %s", code, tc.want)
			}
			if !errors.Is(converted, &tc.api) {
				t.Fatal("error conversion discarded the cause")
			}
		})
	}
}
