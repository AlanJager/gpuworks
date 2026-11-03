package gpu

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAMDMetricsReusePublishedTopologyButReadFreshValues(t *testing.T) {
	p, files := amd395UMAFixture()
	now := time.Unix(100, 0)
	p.clockNow = func() time.Time { return now }
	read, glob := p.readFile, p.glob
	topologyReads := 0
	p.readFile = func(path string) ([]byte, error) {
		if strings.Contains(path, "/kfd/topology/") {
			topologyReads++
		}
		return read(path)
	}
	p.glob = func(path string) ([]string, error) {
		if strings.Contains(path, "/kfd/topology/") || path == "/sys/class/drm/card[0-9]*/device" {
			topologyReads++
		}
		return glob(path)
	}
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	topologyReads = 0
	first, err := p.Metrics(context.Background())
	if err != nil || len(first) != 1 {
		t.Fatalf("first scrape=%v %v", first, err)
	}
	// The production collector samples every ten seconds.
	now = now.Add(10 * time.Second)
	files[umaDevicePath+"/mem_info_gtt_used"] = "8388608"
	files[umaDevicePath+"/gpu_busy_percent"] = "73"
	next, err := p.Metrics(context.Background())
	if err != nil || len(next) != 1 || next[0].Values["gpuworks_gpu_memory_used_bytes"] != 8388608 || next[0].Values["gpuworks_gpu_utilization"] != 73 {
		t.Fatalf("stale dynamic metrics=%v %v", next, err)
	}
	if topologyReads != 0 {
		t.Fatalf("scrapes re-read topology %d times", topologyReads)
	}
	now = now.Add(amdMetricInventoryTTL)
	if _, err := p.Metrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	if topologyReads == 0 {
		t.Fatal("expired static inventory was not revalidated")
	}
}

func TestAMDMetricsChangedIdentityFailsClosedUntilPublishedAgain(t *testing.T) {
	for _, field := range []string{"unique_id", "uevent", "vendor", "device"} {
		t.Run(field, func(t *testing.T) {
			p, files := amd395UMAFixture()
			now := time.Unix(100, 0)
			p.clockNow = func() time.Time { return now }
			published, err := p.Probe(context.Background())
			if err != nil || len(published) != 1 {
				t.Fatalf("probe=%v %v", published, err)
			}
			path := umaDevicePath + "/" + field
			replacements := map[string]string{"unique_id": "9999", "uevent": "PCI_SLOT_NAME=0000:c6:00.0", "vendor": "0x10de", "device": "0x9999"}
			files[path] = replacements[field]
			metrics, err := p.Metrics(context.Background())
			if err != nil || len(metrics) != 0 {
				t.Fatalf("cached inventory authorized changed identity: %v %v", metrics, err)
			}
			now = now.Add(amdMetricInventoryTTL)
			metrics, err = p.Metrics(context.Background())
			if err != nil || len(metrics) != 0 {
				t.Fatalf("telemetry refresh adopted changed identity: %v %v", metrics, err)
			}
			if p.lastSeen[published[0].Index].UUID != published[0].UUID {
				t.Fatal("telemetry advanced published identity")
			}
		})
	}
}

func TestAMDMetricsReplacementDuringReadIsNotPublished(t *testing.T) {
	p, files := amd395UMAFixture()
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	read := p.readFile
	p.readFile = func(path string) ([]byte, error) {
		if path == umaDevicePath+"/gpu_busy_percent" {
			files[umaDevicePath+"/unique_id"] = "9999"
		}
		return read(path)
	}
	metrics, err := p.Metrics(context.Background())
	if err != nil || len(metrics) != 0 {
		t.Fatalf("replacement values used prior identity: %v %v", metrics, err)
	}
}
