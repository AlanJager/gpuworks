package apiclient

import "strings"

type AuthenticationReason string

const (
	AuthAccessKeyInvalid AuthenticationReason = "access_key_invalid"
	AuthSignatureInvalid AuthenticationReason = "signature_invalid"
	AuthLicenseRequired  AuthenticationReason = "license_required"
	AuthClockSkew        AuthenticationReason = "clock_skew"
)

// AccessKeyManagerImpl.doAuth and RestServer.checkTime use the same HTTP 403
// envelope for these failures. Only recognize their known details; never retain
// or expose the remote text, which can contain an AccessKey ID.
func authenticationReason(details string) AuthenticationReason {
	switch details {
	case "wrong accessKey signature":
		return AuthSignatureInvalid
	case "AccessKey authentication requires valid Enterprise license":
		return AuthLicenseRequired
	case "requestTimeTooSkewed":
		return AuthClockSkew
	}
	if strings.HasPrefix(details, "access key id: ") && strings.HasSuffix(details, " does not existed or disabled") {
		return AuthAccessKeyInvalid
	}
	return ""
}
