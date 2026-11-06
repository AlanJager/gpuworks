package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func TestOutboundPolicyWithoutFileIsReadOnlyAndUsesRawStartupValues(t *testing.T) {
	settings := newServerSettings(Config{OutboundRestrictPrivateHosts: false, OutboundAllowPrivateHosts: []string{"configured.internal"}}, nil, nil)
	view, err := settings.OutboundPolicy()
	if err != nil || view.Editable || view.RestrictPrivateHosts || len(view.AllowPrivateHosts) != 1 || view.AllowPrivateHosts[0] != "configured.internal" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	off := false
	_, err = settings.UpdateOutboundPolicy(dto.ServerOutboundPolicyUpdate{RestrictPrivateHosts: &off, AllowPrivateHosts: []string{}, Revision: "ignored"})
	policyErr, ok := err.(*api.ServerSettingsError)
	if !ok || policyErr.Code != api.ErrorServerOutboundPolicyReadOnly {
		t.Fatalf("write error=%v", err)
	}
}

func TestOutboundPolicyUnavailableDoesNotExposeYAMLOrFileContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, []byte("ssh_proxy: [secret-parser-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings := newServerSettings(Config{ConfigPath: path}, nil, nil)
	_, err := settings.OutboundPolicy()
	policyErr, ok := err.(*api.ServerSettingsError)
	if !ok || policyErr.Code != api.ErrorServerOutboundPolicyUnavailable {
		t.Fatalf("read error=%v", err)
	}
	raw, _ := json.Marshal(policyErr)
	if strings.Contains(string(raw), "secret-parser-value") || strings.Contains(string(raw), path) {
		t.Fatalf("unsafe error %s", raw)
	}
}
