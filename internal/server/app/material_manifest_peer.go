//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// confirmRemoteMaterialManifest asks the receiving member to validate its own
// indexed bytes against the currently published source pointer. The response
// is only a verification result; it does not make the member application ready.
func confirmRemoteMaterialManifest(ctx context.Context, client *http.Client, endpoint, digest string, sourceID, targetID uint64) error {
	return callRemoteMaterialManifest(ctx, client, endpoint, digest, sourceID, targetID, "verify")
}

func materializeRemoteMaterialManifest(ctx context.Context, client *http.Client, endpoint, digest string, sourceID, targetID uint64) error {
	return callRemoteMaterialManifest(ctx, client, endpoint, digest, sourceID, targetID, "materialize")
}

func checkRemoteInstalledMaterialManifest(ctx context.Context, client *http.Client, endpoint, digest string, sourceID, targetID uint64) error {
	return callRemoteMaterialManifest(ctx, client, endpoint, digest, sourceID, targetID, "installed")
}

func callRemoteMaterialManifest(ctx context.Context, client *http.Client, endpoint, digest string, sourceID, targetID uint64, action string) error {
	if client == nil || !validMaterialDigest(digest) || sourceID == 0 || targetID == 0 || sourceID == targetID {
		return errors.New("manifest confirmation requires two distinct members and a digest")
	}
	if action != "verify" && action != "materialize" && action != "installed" {
		return errors.New("unknown material manifest action")
	}
	origin, err := url.Parse(endpoint)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.RawPath != "" || (origin.Path != "" && origin.Path != "/") {
		return errors.New("manifest confirmation requires an exact HTTPS member origin")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.VerifyConnection == nil {
		return errors.New("manifest confirmation requires a pinned member TLS verifier")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(endpoint, "/")+materialManifestPath+"/"+digest+"/"+action, nil)
	if err != nil {
		return err
	}
	noRedirect := *client
	// Completed manifest stages send informational responses. Go's absolute
	// header timer does not renew on those responses; the member connection's
	// idle deadline still bounds any interval without real progress.
	manifestTransport := transport.Clone()
	manifestTransport.ResponseHeaderTimeout = 0
	defer manifestTransport.CloseIdleConnections()
	noRedirect.Transport = manifestTransport
	request.Header.Set(materialManifestProgressHeader, "1")
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := noRedirect.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.TLS == nil || len(response.TLS.PeerCertificates) == 0 || (len(response.TLS.VerifiedChains) == 0 && !transport.TLSClientConfig.InsecureSkipVerify) {
		return errors.New("manifest confirmation peer identity was not verified")
	}
	if response.StatusCode != http.StatusNoContent || response.Header.Get("X-Gpuworks-Material-Source") != fmt.Sprint(sourceID) || response.Header.Get("X-Gpuworks-Material-Member") != fmt.Sprint(targetID) || response.Header.Get("X-Gpuworks-Material-Digest") != digest {
		return newMaterialReceiverStatus(response, fmt.Sprintf("manifest receiver rejected current bytes or identity: HTTP %d", response.StatusCode))
	}
	return nil
}
