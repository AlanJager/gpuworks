package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

type runtimeOutboundSettings struct {
	StaticServerSettings
	hosts []string
}

func (s *runtimeOutboundSettings) OutboundAllowPrivateHosts() []string { return s.hosts }

func TestArchivePreviewReportsPolicyDenialWithoutLeakingURL(t *testing.T) {
	origin, _ := archiveFixture(t, []string{"example/private:v1"})
	h := &apiHandlers{}
	response := postJSON(t, h.previewImageArchiveURL, "/api/v1/images/preview-archive-url", fmt.Sprintf(`{"url":%q}`, origin.URL+"/image.tar?token=private-token"), nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var got struct {
		Code   string            `json:"code"`
		Params map[string]string `json:"params"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Code != "IMAGE_ARCHIVE_URL_POLICY_DENIED" || got.Params["config_key"] != "outbound_allow_private_hosts" || got.Params["host"] != strings.TrimPrefix(origin.URL, "http://") {
		t.Fatalf("policy error=%+v", got)
	}
	if strings.Contains(response.Body.String(), "private-token") || strings.Contains(response.Body.String(), "/image.tar") {
		t.Fatalf("policy response leaked the source URL: %s", response.Body.String())
	}
}

func TestArchivePreviewUsesRuntimePermissionInsteadOfOldStaticPermission(t *testing.T) {
	origin, _ := archiveFixture(t, []string{"example/private:v1"})
	settings := &runtimeOutboundSettings{}
	h := &apiHandlers{d: Deps{Server: settings, Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}}}
	for _, stage := range []struct {
		hosts []string
		want  int
	}{{nil, 403}, {[]string{"127.0.0.1"}, 200}, {nil, 403}} {
		settings.hosts = stage.hosts
		response := postJSON(t, h.previewImageArchiveURL, "/api/v1/images/preview-archive-url", fmt.Sprintf(`{"url":%q}`, origin.URL+"/image.tar"), nil)
		if response.Code != stage.want {
			t.Fatalf("runtime hosts=%v status=%d want=%d body=%s", stage.hosts, response.Code, stage.want, response.Body.String())
		}
	}
}
