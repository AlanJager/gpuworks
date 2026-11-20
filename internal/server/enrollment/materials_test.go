package enrollment

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func TestMaterialClientRejectsInvalidPrivateResponses(t *testing.T) {
	for _, name := range []string{"valid", "cluster", "protocol", "authority key", "empty master", "unknown field", "trailing", "oversize", "redirect", "remote error", "canceled"} {
		t.Run(name, func(t *testing.T) {
			var payload []byte
			f := endpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != SecretsPath || r.Header.Get("Authorization") != "" {
					t.Error("unexpected material request contract")
				}
				if name == "redirect" {
					w.Header().Set("Location", "https://example.invalid/private")
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				if name == "remote error" {
					w.WriteHeader(http.StatusForbidden)
				}
				_, _ = w.Write(payload)
			}))
			key, err := identity.NewMemberKey()
			if err != nil {
				t.Fatal(err)
			}
			csr, err := identity.NewMemberCSR(key)
			if err != nil {
				t.Fatal(err)
			}
			fp, err := identity.CSRKeyFingerprint(csr)
			if err != nil {
				t.Fatal(err)
			}
			cert, err := f.issuer.IssueMember(csr, fp, "127.0.0.1:9910", time.Now(), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			result := BootstrapSecrets{ClusterID: f.cluster, Protocol: proto.ClusterProtocolVersion, CACertificatePEM: string(f.ca), CAKeyPEM: string(f.caKey), MasterSecret: []byte("private-response-marker")}
			switch name {
			case "cluster":
				result.ClusterID = strings.Repeat("b", 32)
			case "protocol":
				result.Protocol++
			case "authority key":
				result.CAKeyPEM = string(key)
			case "empty master":
				result.MasterSecret = nil
			}
			payload, err = json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "unknown field":
				payload = append(payload[:len(payload)-1], []byte(`,"extra":true}`)...)
			case "trailing":
				payload = append(payload, []byte(" {}")...)
			case "oversize":
				payload = []byte(strings.Repeat("x", (1<<20)+1))
			}
			ctx := t.Context()
			if name == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := f.client(t).FetchSecrets(ctx, cert, key)
			if name == "valid" {
				if err != nil || !reflect.DeepEqual(got, result) {
					t.Fatal("valid response rejected", err)
				}
				return
			}
			if err == nil || !reflect.DeepEqual(got, BootstrapSecrets{}) {
				t.Fatal("invalid response returned private material")
			}
			if strings.Contains(err.Error(), "private-response-marker") || strings.Contains(err.Error(), "PRIVATE KEY") {
				t.Fatal("private response leaked in error")
			}
		})
	}
}
