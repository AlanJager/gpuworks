package aliyun

import (
	"errors"
	"log/slog"
	"regexp"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// credentialErrorCode classifies the Alibaba Cloud error codes that mean the
// credentials themselves were refused. A key that is accepted but may not
// make the call is a different problem from a wrong key, so it has its own
// code. "" is every other error (throttling, an unsupported endpoint, a
// server-side failure), which is not about the credentials.
func credentialErrorCode(aliyunCode string) string {
	switch aliyunCode {
	case "InvalidAccessKeyId.NotFound", "InvalidAccessKeyId.Inactive":
		return messages.ErrorAliyunAccessKeyInvalid
	case "IncompleteSignature", "SignatureDoesNotMatch":
		return messages.ErrorAliyunSignatureInvalid
	case "Forbidden.RAM", "NoPermission":
		return messages.ErrorAliyunPermissionDenied
	}
	return ""
}

var credentialErrorFallbacks = map[string]string{
	messages.ErrorAliyunAccessKeyInvalid: "The Alibaba Cloud AccessKey ID does not exist or is disabled. Check the AccessKey ID and its state.",
	messages.ErrorAliyunSignatureInvalid: "Alibaba Cloud rejected the request signature. Check that the AccessKey ID and AccessKey Secret are correct and belong together.",
	messages.ErrorAliyunPermissionDenied: "Alibaba Cloud accepted the AccessKey but denied the request. Check the RAM permissions of the user, and of the role when one is set.",
}

// securityTokenHeader matches the STS token a signature error echoes inside
// the canonical request it prints.
var securityTokenHeader = regexp.MustCompile(`(?i)(x-acs-security-token:)[^\n\]]*`)

// credentialError turns a refusal of the credentials into a product error
// with a stable code. Alibaba Cloud's own text (a signature error prints the
// string to sign and the whole canonical request) goes to the server log
// rather than to the person adding the account; the cause stays reachable
// through errors.As. Every other error is returned unchanged.
func credentialError(err error) error {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return err
	}
	code := credentialErrorCode(apiErr.Code)
	if code == "" {
		return err
	}
	slog.Default().Warn("alibaba cloud refused the provider credentials",
		"code", code, "aliyun_code", apiErr.Code, "status", apiErr.Status, "request_id", apiErr.RequestID,
		"message", securityTokenHeader.ReplaceAllString(apiErr.Message, "${1}[REDACTED]"))
	return messages.WrapProductError(code, credentialErrorFallbacks[code], nil, err)
}
