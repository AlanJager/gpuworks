package aliyun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
)

// signatureMismatchBody is what Alibaba Cloud answers (HTTP 400) to a V3
// request whose signature it cannot reproduce: the message prints the string
// to sign and the canonical request the server computed.
func signatureMismatchBody(req signedRequest) map[string]string {
	headers := map[string]string{"host": req.host}
	for name, value := range req.headers {
		headers[name] = value
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	canonical := req.method + "\n" + canonicalPath(req.path) + "\n" + canonicalQuery(req.query) + "\n"
	for _, name := range names {
		canonical += name + ":" + headers[name] + "\n"
	}
	canonical += "\n" + strings.Join(names, ";") + "\n" + emptyPayloadHash
	digest := sha256.Sum256([]byte(canonical))
	return map[string]string{
		"RequestId": "5C2B4A1E-93D0-5F0B-8C55-0D6E0F1A2B3C",
		"Message":   "Specified signature does not match our calculation. server StringToSign is [" + signAlgorithm + "\n" + hex.EncodeToString(digest[:]) + "], server CanonicalRequest is [" + canonical + "]",
		"Recommend": "https://api.aliyun.com/troubleshoot?q=IncompleteSignature&product=Ecs&requestId=5C2B4A1E-93D0-5F0B-8C55-0D6E0F1A2B3C",
		"HostId":    req.host,
		"Code":      "IncompleteSignature",
	}
}

// unknownAccessKeyBody is the HTTP 404 answer to an AccessKey ID that does
// not exist.
func unknownAccessKeyBody(host string) map[string]string {
	return aliyunErrorBody(host, "InvalidAccessKeyId.NotFound", "Specified access key is not found.")
}

func aliyunErrorBody(host, code, message string) map[string]string {
	return map[string]string{
		"RequestId": "7A1D0B52-1E0C-5B7F-9E21-4C3F2A1B0D9E",
		"Message":   message,
		"Recommend": "https://api.aliyun.com/troubleshoot?q=" + code + "&product=Ecs&requestId=7A1D0B52-1E0C-5B7F-9E21-4C3F2A1B0D9E",
		"HostId":    host,
		"Code":      code,
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func assertCredentialError(t *testing.T, err error, code, aliyunCode string) {
	t.Helper()
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != code {
		t.Fatalf("code = %v, want %s", err, code)
	}
	if err.Error() != credentialErrorFallbacks[code] || credentialErrorFallbacks[code] == "" {
		t.Fatalf("text = %q", err.Error())
	}
	for _, raw := range []string{"StringToSign", "CanonicalRequest", "x-acs-", "RequestId", "aliyun:"} {
		if strings.Contains(err.Error(), raw) {
			t.Fatalf("text carries Alibaba Cloud's own wording %q: %q", raw, err.Error())
		}
	}
	if ErrorCode(err) != aliyunCode {
		t.Fatalf("cause lost: Alibaba Cloud code = %q, want %s", ErrorCode(err), aliyunCode)
	}
}

// The fake checks the signature like the real API, so these two are the real
// conversations: a wrong secret and an AccessKey ID that does not exist.
func TestValidateCredentialsNamesAWrongSecretAndAnUnknownKey(t *testing.T) {
	logs := captureLogs(t)
	calls := 0
	d, _ := newFake(t, func(fakeCall) (int, any) { calls++; return 200, regionsBody("cn-shanghai") })

	err := d.ValidateCredentials(context.Background(), provider.Credentials{PublicKey: "LTAIuser", PrivateKey: "wrong-secret"})
	assertCredentialError(t, err, "ALIYUN_SIGNATURE_INVALID", "IncompleteSignature")
	logged := logs.String()
	for _, want := range []string{"aliyun_code=IncompleteSignature", "status=400", "request_id=5C2B4A1E-93D0-5F0B-8C55-0D6E0F1A2B3C", "server StringToSign is [ACS3-HMAC-SHA256", "x-acs-action:DescribeRegions"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log lost %q: %s", want, logged)
		}
	}
	if strings.Contains(logged, "wrong-secret") || strings.Contains(logged, "user-secret") {
		t.Fatalf("a secret reached the log: %s", logged)
	}

	logs.Reset()
	err = d.ValidateCredentials(context.Background(), provider.Credentials{PublicKey: "LTAInobody", PrivateKey: "user-secret"})
	assertCredentialError(t, err, "ALIYUN_ACCESS_KEY_INVALID", "InvalidAccessKeyId.NotFound")
	if !strings.Contains(logs.String(), "aliyun_code=InvalidAccessKeyId.NotFound") || !strings.Contains(logs.String(), "status=404") {
		t.Fatalf("log = %s", logs.String())
	}
	if calls != 0 {
		t.Fatalf("%d calls got past the signature check", calls)
	}
}

func TestValidateCredentialsClassifiesRefusals(t *testing.T) {
	for _, tc := range []struct {
		aliyunCode, message string
		status              int
		want                string
	}{
		{"InvalidAccessKeyId.Inactive", "Specified access key is disabled.", 403, "ALIYUN_ACCESS_KEY_INVALID"},
		{"SignatureDoesNotMatch", "Specified signature is not matched with our calculation. server string to sign is:POST&%2F&AccessKeyId%3DLTAIuser", 400, "ALIYUN_SIGNATURE_INVALID"},
		{"Forbidden.RAM", "User not authorized to operate on the specified resource, or this API doesn't support RAM.", 403, "ALIYUN_PERMISSION_DENIED"},
		{"NoPermission", "You are not authorized to do this action. You should be authorized by RAM.", 403, "ALIYUN_PERMISSION_DENIED"},
	} {
		t.Run(tc.aliyunCode, func(t *testing.T) {
			logs := captureLogs(t)
			attempts := 0
			d, _ := newFake(t, func(c fakeCall) (int, any) {
				attempts++
				return tc.status, aliyunErrorBody(c.host, tc.aliyunCode, tc.message)
			})
			err := d.ValidateCredentials(context.Background(), userCreds)
			assertCredentialError(t, err, tc.want, tc.aliyunCode)
			if attempts != 1 {
				t.Fatalf("a refusal was retried: %d attempts", attempts)
			}
			if !strings.Contains(logs.String(), "aliyun_code="+tc.aliyunCode) {
				t.Fatalf("log = %s", logs.String())
			}
		})
	}
}

// A role's key is refused at AssumeRole, before any ECS call; the refusal is
// the same product error.
func TestValidateCredentialsClassifiesARefusedRole(t *testing.T) {
	captureLogs(t)
	d, _ := newFake(t, func(c fakeCall) (int, any) {
		if c.action == "AssumeRole" {
			return 403, aliyunErrorBody(c.host, "NoPermission", "You are not authorized to do this action. You should be authorized by RAM.")
		}
		return 200, regionsBody("cn-shanghai")
	})
	creds := provider.Credentials{PublicKey: "LTAIuser", PrivateKey: "user-secret", Extra: map[string]string{"role_arn": "acs:ram::1234567890123456:role/gpuworks"}}
	assertCredentialError(t, d.ValidateCredentials(context.Background(), creds), "ALIYUN_PERMISSION_DENIED", "NoPermission")
}

// Errors that are not about the credentials keep their own text and no
// product code: throttling that outlasts the retries, a server-side failure,
// an unreachable endpoint, a cancelled request.
func TestValidateCredentialsLeavesOtherErrorsAlone(t *testing.T) {
	logs := captureLogs(t)
	plain := func(err error) {
		t.Helper()
		var product *messages.ProductError
		if err == nil || errors.As(err, &product) {
			t.Fatalf("err = %v", err)
		}
	}
	d, _ := newFake(t, func(c fakeCall) (int, any) {
		return 400, aliyunErrorBody(c.host, "Throttling.User", "Request was denied due to user flow control.")
	})
	err := d.ValidateCredentials(context.Background(), userCreds)
	plain(err)
	if !strings.Contains(err.Error(), "aliyun: Throttling.User: Request was denied due to user flow control. (HTTP 400, request ") {
		t.Fatalf("throttling text = %v", err)
	}

	d, _ = newFake(t, func(c fakeCall) (int, any) {
		return 500, aliyunErrorBody(c.host, "InternalError", "The request processing has failed due to some unknown error.")
	})
	err = d.ValidateCredentials(context.Background(), userCreds)
	plain(err)
	if ErrorCode(err) != "InternalError" {
		t.Fatalf("server failure = %v", err)
	}

	d, _ = newFake(t, func(fakeCall) (int, any) { return 200, regionsBody("cn-shanghai") }, centralECSHost)
	err = d.ValidateCredentials(context.Background(), userCreds)
	plain(err)
	if ErrorCode(err) != "" || !strings.Contains(err.Error(), "DescribeRegions "+centralECSHost) {
		t.Fatalf("transport failure = %v", err)
	}

	d, _ = newFake(t, func(fakeCall) (int, any) { return 200, regionsBody("cn-shanghai") })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.ValidateCredentials(ctx, userCreds); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled = %v", err)
	}
	if err := d.ValidateCredentials(context.Background(), userCreds); err != nil {
		t.Fatalf("valid credentials = %v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("an error that is not a refusal was logged as one: %s", logs.String())
	}
}

// With a role the canonical request a signature error prints carries the STS
// token, which is a secret while it lasts.
func TestCredentialErrorLogRedactsTheSecurityToken(t *testing.T) {
	logs := captureLogs(t)
	body := signatureMismatchBody(signedRequest{method: "POST", host: centralECSHost, path: "/", headers: map[string]string{
		"x-acs-action": "DescribeRegions", "x-acs-security-token": "CAIS-temporary-token", "x-acs-version": "2014-05-26",
	}})
	err := credentialError(&apiError{Status: 400, Code: body["Code"], Message: body["Message"], RequestID: body["RequestId"]})
	assertCredentialError(t, err, "ALIYUN_SIGNATURE_INVALID", "IncompleteSignature")
	logged := logs.String()
	if strings.Contains(logged, "CAIS-temporary-token") || !strings.Contains(logged, "x-acs-security-token:[REDACTED]") || !strings.Contains(logged, "x-acs-version:2014-05-26") {
		t.Fatalf("log = %s", logged)
	}
}
