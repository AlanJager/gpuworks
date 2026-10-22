//go:build multiproc

package l2

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPImageForceRefetchesURLArchive(t *testing.T) {
	for _, relay := range []bool{false, true} {
		name := "direct"
		source := "url"
		if relay {
			name, source = "relay", "url-relay"
		}
		t.Run(name, func(t *testing.T) {
			c := startArchiveImportCluster(t, "gpustack/l2-force-runner:"+name)
			version := c.importArchiveImage(t, "l2-force-"+name)
			first := c.nodeRow(t, c.fetchOnNode(t, version.ID, relay))
			if first.State != "present" || first.TransferID != "" || first.TransferredBytes != int64(len(c.archive)) {
				t.Fatalf("initial archive load did not settle: %+v", first)
			}
			archiveGets := func() int {
				n := 0
				for _, req := range c.site.Requests() {
					if strings.HasPrefix(req, "GET /image.tar") && (!relay || !strings.Contains(req, "bytes=")) {
						n++
					}
				}
				return n
			}
			before := archiveGets()
			if before == 0 || (relay && before != 1) {
				t.Fatalf("initial load did not fetch the archive: requests=%v", c.site.Requests())
			}
			for _, force := range []bool{false, true} {
				body, _ := json.Marshal(map[string]any{"source": "url", "archive_url": c.archiveURL, "relay": relay, "force": force})
				resp, err := authedPost(t, c.base, c.adminToken, fmt.Sprintf("/api/v1/images/%d/nodes/%s/sync", version.ID, c.node.UUID), body)
				if err != nil {
					t.Fatal(err)
				}
				var accepted struct {
					State      string `json:"state"`
					TransferID string `json:"transfer_id"`
				}
				decodeErr := json.NewDecoder(resp.Body).Decode(&accepted)
				resp.Body.Close()
				if resp.StatusCode != http.StatusAccepted || decodeErr != nil {
					t.Fatalf("sync force=%v: status=%d response=%+v err=%v", force, resp.StatusCode, accepted, decodeErr)
				}
				if !force {
					if accepted.State != "present" || accepted.TransferID != "" || archiveGets() != before {
						t.Fatalf("ordinary sync refetched a present image: response=%+v gets=%d", accepted, archiveGets())
					}
					continue
				}
				if accepted.State != "syncing" || accepted.TransferID == "" {
					t.Fatalf("forced sync reused the present row: %+v", accepted)
				}
			}
			deadline := time.Now().Add(60 * time.Second)
			for {
				row := c.nodeRow(t, findArchiveImportVersion(t, c.base, c.adminToken, c.imageRef))
				if row.State == "failed" {
					t.Fatalf("forced archive load failed: %+v", row)
				}
				if archiveGets() > before && (!relay || archiveGets() == before+1) && row.State == "present" && row.TransferID == "" && row.Source == source && row.TransferredBytes == int64(len(c.archive)) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("forced archive load did not settle: row=%+v gets=%d", row, archiveGets())
				}
				time.Sleep(25 * time.Millisecond)
			}
			assertNoStagedArchives(t, c.dataDir)
			assertNoStagedArchives(t, c.agentState)
		})
	}
}
