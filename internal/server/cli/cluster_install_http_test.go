package cli

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestClusterInstallAuthenticatedBusinessWireAndFailures(t *testing.T) {
	for _, failure := range []string{"", "login", "identity", "nodes", "nodes-null", "settings", "defaults", "put", "ack", "replication", "json"} {
		t.Run(failure, func(t *testing.T) {
			var events []string
			stored := ""
			key := "cluster_install_literal"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				events = append(events, r.Method+" "+r.URL.Path)
				if r.URL.Path == "/api/v1/auth/login" {
					if r.Method != "POST" || r.Header.Get("Authorization") != "" {
						t.Error("wrong login wire")
					}
					var credentials map[string]string
					_ = json.NewDecoder(r.Body).Decode(&credentials)
					if credentials["username"] != "admin" || credentials["password"] != "private-password" {
						t.Error("wrong login defaults")
					}
					if failure == "login" {
						w.WriteHeader(401)
						fmt.Fprint(w, `{"error":"private-secret"}`)
						return
					}
					identity := "admin"
					if failure == "identity" {
						identity = "operator"
					}
					fmt.Fprintf(w, `{"token":"header.claims.signature","user":{"id":1,"username":%q,"role":"admin"}}`, identity)
					return
				}
				if r.Header.Get("Authorization") != "Bearer header.claims.signature" {
					t.Error("business token omitted")
				}
				switch r.URL.Path {
				case "/api/v1/nodes":
					if failure == "nodes" {
						w.WriteHeader(503)
						fmt.Fprint(w, `{}`)
					} else if failure == "nodes-null" {
						fmt.Fprint(w, `{"items":null,"total":0}`)
					} else {
						fmt.Fprint(w, `{"items":[],"total":0}`)
					}
				case "/api/v1/settings":
					if failure == "settings" {
						w.WriteHeader(503)
						fmt.Fprint(w, `{}`)
						return
					}
					if failure == "json" {
						fmt.Fprint(w, `not-json`)
						return
					}
					result := map[string]any{"deployment": map[string]any{"pending_timeout_sec": 600, "start_timeout_sec": 1800}}
					if failure == "defaults" {
						result["deployment"] = nil
					}
					if failure == "replication" {
						result[key] = "different"
					} else if stored != "" {
						result[key] = stored
					}
					_ = json.NewEncoder(w).Encode(result)
				case "/api/v1/settings/" + key:
					if r.Method != "PUT" {
						t.Error("write was not PUT")
					}
					if err := json.NewDecoder(r.Body).Decode(&stored); err != nil {
						t.Error("settings API expects a JSON string, not a value object")
					}
					if failure == "put" {
						w.WriteHeader(503)
						fmt.Fprint(w, `{}`)
					} else if failure == "ack" {
						fmt.Fprint(w, `{"status":"pending"}`)
					} else {
						fmt.Fprint(w, `{"status":"saved"}`)
					}
				default:
					t.Errorf("unexpected external path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			host, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
			number, _ := strconv.Atoi(port)
			t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
			t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
			t.Setenv("NO_PROXY", "")
			req := clusterInstallNodeRequest{Host: host, HTTP: number, Password: "private-password"}
			err := clusterInstallBusiness(t.Context(), req, key, "independent-value", true)
			if failure == "" {
				if err != nil {
					t.Fatal(err)
				}
				if len(events) != 5 || stored != "independent-value" {
					t.Fatalf("five operations were not proved: %v", events)
				}
			} else if err == nil {
				t.Fatal("business failure was reported ready")
			}
			if err != nil && strings.Contains(err.Error(), "private-secret") {
				t.Fatal("HTTP failure disclosed body")
			}
		})
	}
}

func TestClusterInstallInvitationUsesPinnedCAAndAuthorizedOrigin(t *testing.T) {
	expectedOrigin := ""
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/cluster/invitations" || r.Header.Get("Authorization") != "Bearer admin-token" {
			t.Error("wrong invitation request")
		}
		var request map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&request)
		if string(request["valid_for_seconds"]) != "3600" || string(request["authorized_enrollment_origin"]) != strconv.Quote(expectedOrigin) {
			t.Errorf("wrong literal invitation: %v", request)
		}
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"cluster_id":"cluster","ca_fingerprint":"pin","protocol":1,"enrollment_url":"https://seed:9443","authorized_enrollment_origin":%q,"token":"private-token"}`, expectedOrigin)
	}))
	defer server.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	number, _ := strconv.Atoi(port)
	expectedOrigin = "https://192.0.2.2:" + port
	dir := t.TempDir()
	caDir := filepath.Join(dir, "data", "cluster-enrollment")
	if err := os.MkdirAll(caDir, 0700); err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caDir+"/ca.pem", ca, 0600); err != nil {
		t.Fatal(err)
	}
	req := clusterInstallNodeRequest{Host: host, Enrollment: number, Dir: dir}
	invitation, err := clusterInstallInvite(t.Context(), req, "admin-token", "192.0.2.2")
	if err != nil || invitation.Token != "private-token" || invitation.AuthorizedEnrollmentOrigin != expectedOrigin {
		t.Fatalf("trusted invitation failed: %v", err)
	}
	untrusted, _ := clusterInstallHTTPClient(nil)
	defer untrusted.CloseIdleConnections()
	if err := clusterInstallHTTP(context.Background(), untrusted, "POST", server.URL, "", nil, &invitation, 201); err == nil {
		t.Fatal("untrusted enrollment certificate accepted")
	}
}

func TestClusterInstallHTTPRefusesRedirectsAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://127.0.0.1:1/private", 302) }))
	defer server.Close()
	client, _ := clusterInstallHTTPClient(nil)
	defer client.CloseIdleConnections()
	var value any
	if err := clusterInstallHTTP(t.Context(), client, "GET", server.URL, "private-token", nil, &value, 200); err == nil {
		t.Fatal("redirect followed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := clusterInstallHTTP(ctx, client, "GET", server.URL, "private-token", nil, &value, 200); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}
