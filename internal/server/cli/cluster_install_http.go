package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func clusterInstallHTTP(ctx context.Context, client *http.Client, method, address, token string, payload any, dest any, wantStatus int) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("%s operation could not complete", method)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		return fmt.Errorf("%s returned HTTP %d (expected %d)", method, response.StatusCode, wantStatus)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return errors.New("installation API response exceeds limit")
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return errors.New("installation API returned invalid JSON")
	}
	return nil
}

func clusterInstallHTTPClient(ca []byte) (*http.Client, error) {
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if ca != nil {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return nil, errors.New("invalid cluster CA")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	return &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("installation API redirects are refused")
	}}, nil
}

func clusterInstallLogin(ctx context.Context, req clusterInstallNodeRequest) (string, error) {
	client, _ := clusterInstallHTTPClient(nil)
	defer client.CloseIdleConnections()
	var session struct {
		Token string `json:"token"`
		User  struct {
			ID             int64 `json:"id"`
			Username, Role string
		} `json:"user"`
	}
	address := "http://" + net.JoinHostPort(req.Host, fmt.Sprint(req.HTTP)) + "/api/v1/auth/login"
	err := clusterInstallHTTP(ctx, client, "POST", address, "", map[string]string{"username": "admin", "password": req.Password}, &session, 200)
	if err != nil {
		return "", err
	}
	pieces := strings.Split(session.Token, ".")
	if len(pieces) != 3 || pieces[0] == "" || pieces[1] == "" || pieces[2] == "" || session.User.ID <= 0 || session.User.Username != "admin" || session.User.Role != "admin" {
		return "", errors.New("unexpected installation login identity")
	}
	return session.Token, nil
}

func clusterInstallInvite(ctx context.Context, req clusterInstallNodeRequest, token, host string) (dto.ClusterInvitationResponse, error) {
	var result dto.ClusterInvitationResponse
	ca, err := os.ReadFile(req.Dir + "/data/cluster-enrollment/ca.pem")
	if err != nil {
		return result, err
	}
	client, err := clusterInstallHTTPClient(ca)
	if err != nil {
		return result, err
	}
	defer client.CloseIdleConnections()
	address := "https://" + net.JoinHostPort(req.Host, fmt.Sprint(req.Enrollment)) + "/api/v1/cluster/invitations"
	err = clusterInstallHTTP(ctx, client, "POST", address, token, map[string]any{"valid_for_seconds": 3600, "authorized_enrollment_origin": "https://" + net.JoinHostPort(host, fmt.Sprint(req.Enrollment))}, &result, 201)
	return result, err
}

func clusterInstallBusiness(ctx context.Context, req clusterInstallNodeRequest, key, value string, write bool) error {
	token, err := clusterInstallLogin(ctx, req)
	if err != nil {
		return err
	}
	client, _ := clusterInstallHTTPClient(nil)
	defer client.CloseIdleConnections()
	base := "http://" + net.JoinHostPort(req.Host, fmt.Sprint(req.HTTP)) + "/api/v1"
	// The node list is a page: {"items":[...],"total":N}.
	var nodes struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := clusterInstallHTTP(ctx, client, "GET", base+"/nodes", token, nil, &nodes, 200); err != nil {
		return err
	}
	if nodes.Items == nil || len(nodes.Items) != 0 {
		return errors.New("fresh installation returned unexpected nodes")
	}
	read := func() (map[string]json.RawMessage, error) {
		var values map[string]json.RawMessage
		err := clusterInstallHTTP(ctx, client, "GET", base+"/settings", token, nil, &values, 200)
		if err != nil {
			return nil, err
		}
		// Every initialized database carries the deployment policy document.
		var policy map[string]json.RawMessage
		if err := json.Unmarshal(values["deployment"], &policy); err != nil || policy == nil {
			return nil, errors.New("settings defaults are missing")
		}
		return values, nil
	}
	if _, err := read(); err != nil {
		return err
	}
	if write {
		var response map[string]string
		if err := clusterInstallHTTP(ctx, client, "PUT", base+"/settings/"+url.PathEscape(key), token, value, &response, 200); err != nil {
			return err
		}
		if response["status"] != "saved" {
			return errors.New("setting commit was not acknowledged")
		}
	}
	values, err := read()
	if err != nil {
		return err
	}
	var actual string
	if err := json.Unmarshal(values[key], &actual); err != nil || actual != value {
		return errors.New("committed setting does not match across control-plane entries")
	}
	return nil
}
