package cloud

import (
	"errors"
	"net"
	"syscall"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
)

func connectionErrorCode(err error) string {
	var dns *net.DNSError
	switch {
	case errors.As(err, &dns):
		return messages.ErrorZStackDNSFailed
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return messages.ErrorZStackIPUnreachable
	case errors.Is(err, syscall.ECONNREFUSED):
		return messages.ErrorZStackPortRefused
	}
	return ""
}

func credentialError(err error, diagnosis *endpointDiagnosis) error {
	code := messages.ErrorZStackRequestFailed
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Kind == apiclient.ErrorAuthentication:
			code = messages.ErrorZStackAuthRejected
			switch apiErr.AuthReason {
			case apiclient.AuthAccessKeyInvalid:
				code = messages.ErrorZStackAccessKeyInvalid
			case apiclient.AuthSignatureInvalid:
				code = messages.ErrorZStackSignatureInvalid
			case apiclient.AuthLicenseRequired:
				code = messages.ErrorZStackLicenseRequired
			case apiclient.AuthClockSkew:
				code = messages.ErrorZStackClockSkew
			}
		case apiErr.ViaProxy && apiErr.Phase == apiclient.PhaseConnect:
			code = messages.ErrorZStackProxyConnectionFailed
		case apiErr.Phase == apiclient.PhaseTLS:
			code = messages.ErrorZStackTLSFailed
		case apiErr.Phase == apiclient.PhaseResponse && apiErr.Kind == apiclient.ErrorTimeout:
			code = messages.ErrorZStackResponseTimeout
		case apiErr.Phase == apiclient.PhaseConnect:
			if reason := connectionErrorCode(err); reason != "" {
				code = reason
			}
		case apiErr.Kind == apiclient.ErrorTimeout:
			code = messages.ErrorZStackRequestTimeout
		}
	}
	if diagnosis != nil {
		switch {
		case diagnosis.tcpError == nil:
			code = messages.ErrorZStackConnectionRecovered
		case connectionErrorCode(diagnosis.tcpError) != "":
			code = connectionErrorCode(diagnosis.tcpError)
		case diagnosis.ip == ipResponded:
			code = messages.ErrorZStackPortUnreachable
		case diagnosis.ip == ipNoReply:
			code = messages.ErrorZStackNetworkUnconfirmed
		default:
			code = messages.ErrorZStackNetworkDiagnosticUnavailable
		}
	}
	return messages.WrapProductError(code, credentialErrorFallbacks[code], nil, err)
}

var credentialErrorFallbacks = map[string]string{
	messages.ErrorZStackRequestFailed:                "ZStack API validation failed. Check the endpoint, service and API permissions.",
	messages.ErrorZStackRequestTimeout:               "The ZStack API request timed out. Check network connectivity and the service response.",
	messages.ErrorZStackDNSFailed:                    "The ZStack hostname could not be resolved. Check the address and DNS configuration.",
	messages.ErrorZStackIPUnreachable:                "GPUWorks cannot reach the ZStack IP. Check the address and network route.",
	messages.ErrorZStackPortRefused:                  "The ZStack port refused the connection. Check the port and whether the service is running.",
	messages.ErrorZStackAccessKeyInvalid:             "The ZStack AK does not exist or is disabled. Check the AccessKey ID and its state.",
	messages.ErrorZStackSignatureInvalid:             "ZStack rejected the AK/SK signature. Check that the AccessKey ID and Secret match.",
	messages.ErrorZStackLicenseRequired:              "ZStack AccessKey authentication requires a valid Enterprise license.",
	messages.ErrorZStackClockSkew:                    "ZStack rejected the request time. Synchronize the GPUWorks and ZStack server clocks.",
	messages.ErrorZStackAuthRejected:                 "ZStack rejected access. Check the AK/SK, account permissions and authentication configuration.",
	messages.ErrorZStackProxyConnectionFailed:        "The ZStack request failed while connecting through a proxy. Check the proxy and its network route.",
	messages.ErrorZStackTLSFailed:                    "The ZStack TLS handshake failed. Check HTTPS and the server certificate.",
	messages.ErrorZStackResponseTimeout:              "Connected to ZStack, but the API response timed out. Check the service load and response path.",
	messages.ErrorZStackConnectionRecovered:          "The ZStack API connection failed, but a follow-up TCP connection succeeded. Retry to validate the API and AK/SK.",
	messages.ErrorZStackPortUnreachable:              "The ZStack IP responded, but the port could not be connected. Check the port, service and firewall.",
	messages.ErrorZStackNetworkUnconfirmed:           "The ZStack IP probe received no reply and the port could not be connected. Check the address, port and firewall; the cause is not yet determined.",
	messages.ErrorZStackNetworkDiagnosticUnavailable: "The ZStack connection failed and the IP probe could not run. Check the address, port and firewall.",
}
