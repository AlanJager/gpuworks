//go:build cloudgpu

package l4

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

func skipIfNoSecrets(t *testing.T, names ...string) map[string]string {
	t.Helper()
	values := readCloudSecrets()
	for _, name := range names {
		values[name] = strings.TrimSpace(values[name])
	}
	for _, name := range names {
		if values[name] == "" {
			message := fmt.Sprintf("%s is not configured; skipping L4 test", name)
			if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
				t.Fatal(message)
			}
			t.Skip(message)
		}
	}
	return values
}

func readCloudSecrets() map[string]string {
	values := map[string]string{}
	for _, name := range []string{"RUNPOD_API_KEY", "RUNPOD_KEY", "RUNPOD_POD_ID", "RUNPOD_PUBLIC_IP", "RUNPOD_SSH_PORT", "RUNPOD_SSH_USER", "RUNPOD_PRIVATE_KEY", "RUNPOD_SSH_PRIVATE_KEY", "COMPSHARE_ACCESS_KEY", "COMPSHARE_SECRET_KEY", "COMPSHARE_KEY", "COMPSHARE_IMAGE_ID", "COMPSHARE_GOLDEN_IMAGE_ID", "COMPSHARE_CONTAINER_IMAGE_ID", "VAST_API_KEY", "VAST_SSH_PRIVATE_KEY", "VAST_SSH_KEY_PATH", "VAST_MAX_HOURLY_PRICE", "VAST_TEST_IMAGE", "AUTODL_TOKEN", "AUTODL_IMAGE_UUID", "AUTODL_MAX_HOURLY_PRICE", "GITEE_API_TOKEN", "GITEE_ZONE", "GITEE_IMAGE", "GITEE_GPU_MODEL"} {
		values[name] = strings.TrimSpace(os.Getenv(name))
	}
	// Per-region image variables are open-ended (one per CompShare region).
	for _, kv := range os.Environ() {
		if name, value, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "COMPSHARE_ZONE_IMAGE_") {
			values[name] = strings.TrimSpace(value)
		}
	}
	for _, path := range findSecretsFiles() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if ok && values[key] == "" {
				values[key] = strings.Trim(strings.TrimSpace(value), "\"'")
			}
		}
	}
	if values["RUNPOD_API_KEY"] == "" {
		values["RUNPOD_API_KEY"] = values["RUNPOD_KEY"]
	}
	if values["COMPSHARE_ACCESS_KEY"] == "" || values["COMPSHARE_SECRET_KEY"] == "" {
		combined := values["COMPSHARE_KEY"]
		sep := "."
		if strings.Contains(combined, ":") {
			sep = ":"
		}
		if parts := strings.SplitN(combined, sep, 2); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			if values["COMPSHARE_ACCESS_KEY"] == "" {
				values["COMPSHARE_ACCESS_KEY"] = parts[0]
			}
			if values["COMPSHARE_SECRET_KEY"] == "" {
				values["COMPSHARE_SECRET_KEY"] = parts[1]
			}
		}
	}
	return values
}

func findSecretsFiles() []string {
	dir, err := os.Getwd()
	if err != nil {
		return []string{filepath.Join("secrets", "cloud-providers.env")}
	}
	var files []string
	for {
		for _, name := range []string{"cloud-providers.env", "cloud.env", "ascend.env"} {
			candidate := filepath.Join(dir, "secrets", name)
			if _, err := os.Stat(candidate); err == nil {
				files = append(files, candidate)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if len(files) == 0 {
		return []string{filepath.Join(dir, "secrets", "cloud-providers.env")}
	}
	return files
}

func runpodProvider(t *testing.T) *cloudprovider.Runpod {
	t.Helper()
	secrets := readCloudSecrets()
	manualKey := strings.TrimSpace(secrets["RUNPOD_PRIVATE_KEY"])
	if manualKey == "" {
		manualKey = strings.TrimSpace(secrets["RUNPOD_SSH_PRIVATE_KEY"])
	}
	if manualKey != "" && !strings.Contains(manualKey, "BEGIN") {
		if data, err := os.ReadFile(manualKey); err == nil {
			manualKey = string(data)
		}
	}
	if secrets["RUNPOD_POD_ID"] != "" && secrets["RUNPOD_PUBLIC_IP"] != "" && secrets["RUNPOD_SSH_PORT"] != "" && manualKey != "" {
		port, err := strconv.Atoi(secrets["RUNPOD_SSH_PORT"])
		if err != nil {
			t.Fatalf("invalid RUNPOD_SSH_PORT: %v", err)
		}
		p, err := cloudprovider.NewRunpodManual(secrets["RUNPOD_POD_ID"], secrets["RUNPOD_PUBLIC_IP"], port, secrets["RUNPOD_SSH_USER"], []byte(manualKey))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if strings.TrimSpace(secrets["RUNPOD_API_KEY"]) == "" {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("RUNPOD_API_KEY or complete manual RunPod SSH settings are required")
		}
		skipOrFailExternal(t, "RUNPOD_API_KEY or complete manual RunPod SSH settings are not configured")
	}
	runID := cloudTestRunID()
	if runID == "" {
		var err error
		runID, err = cloudprovider.NewRunID()
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := cloudprovider.NewRunpod(secrets["RUNPOD_API_KEY"], runID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func compshareProvider(t *testing.T) *cloudprovider.Compshare {
	t.Helper()
	secrets := skipIfNoSecrets(t, "COMPSHARE_ACCESS_KEY", "COMPSHARE_SECRET_KEY")
	imageID := compshareImageID(secrets)
	if imageID == "" {
		skipOrFailExternal(t, "COMPSHARE_GOLDEN_IMAGE_ID or COMPSHARE_IMAGE_ID is required")
	}
	runID, err := cloudProviderRunID()
	if err != nil {
		t.Fatal(err)
	}
	zoneImages := compshareZoneImages(secrets)
	// Which image a run booted decides which GPUs can work (see
	// prepare-compshare-golden); the IDs are not secret.
	t.Logf("CompShare images: default=%s per-region=%v", imageID, zoneImages)
	p, err := cloudprovider.NewCompshareZoneImages(secrets["COMPSHARE_ACCESS_KEY"], secrets["COMPSHARE_SECRET_KEY"], imageID, zoneImages, runID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// compshareZoneImages turns COMPSHARE_ZONE_IMAGE_<REGION> secrets (e.g.
// COMPSHARE_ZONE_IMAGE_CN_SH2) into a region→image map. CompShare custom
// images are region-scoped (cn-sh2-02 does not sync them), so tests that
// iterate both VM regions need a region-local golden or the create fails
// with "availability zone does not support this image".
func compshareZoneImages(secrets map[string]string) map[string]string {
	var zoneImages map[string]string
	for name, value := range secrets {
		region, ok := strings.CutPrefix(name, "COMPSHARE_ZONE_IMAGE_")
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		if zoneImages == nil {
			zoneImages = make(map[string]string)
		}
		zoneImages[strings.ToLower(strings.ReplaceAll(region, "_", "-"))] = strings.TrimSpace(value)
	}
	return zoneImages
}

// compshareContainerProvider is compshareProvider able to rent the container
// product, which boots from its own image (COMPSHARE_CONTAINER_IMAGE_ID).
func compshareContainerProvider(t *testing.T) *cloudprovider.Compshare {
	t.Helper()
	p := compshareProvider(t)
	image := strings.TrimSpace(readCloudSecrets()["COMPSHARE_CONTAINER_IMAGE_ID"])
	if image == "" {
		skipOrFailExternal(t, "COMPSHARE_CONTAINER_IMAGE_ID is required to rent a CompShare container instance")
	}
	return p.WithContainerImage(image)
}

func compshareImageID(secrets map[string]string) string {
	if golden := strings.TrimSpace(secrets["COMPSHARE_GOLDEN_IMAGE_ID"]); golden != "" {
		return golden
	}
	return strings.TrimSpace(secrets["COMPSHARE_IMAGE_ID"])
}

func vastProvider(t *testing.T) *cloudprovider.Vast {
	t.Helper()
	secrets := skipIfNoSecrets(t, "VAST_API_KEY")
	privateKey, err := vastPrivateKey(secrets)
	if err != nil {
		t.Fatal(err)
	}
	if len(privateKey) == 0 {
		privateKey, _, err = cloudprovider.GenerateSSHKey()
		if err != nil {
			t.Fatalf("generate ephemeral Vast SSH key: %v", err)
		}
	}
	runID, err := cloudProviderRunID()
	if err != nil {
		t.Fatal(err)
	}
	p, err := cloudprovider.NewVast(secrets["VAST_API_KEY"], runID, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func autodlProvider(t *testing.T) *cloudprovider.AutoDL {
	t.Helper()
	secrets := skipIfNoSecrets(t, "AUTODL_TOKEN")
	for _, name := range []string{"AUTODL_IMAGE_UUID", "AUTODL_MAX_HOURLY_PRICE"} {
		if value := strings.TrimSpace(secrets[name]); value != "" && strings.TrimSpace(os.Getenv(name)) == "" {
			t.Setenv(name, value)
		}
	}
	runID, err := cloudProviderRunID()
	if err != nil {
		t.Fatal(err)
	}
	p, err := cloudprovider.NewAutoDL(secrets["AUTODL_TOKEN"], runID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func cloudProviderRunID() (string, error) {
	if runID := cloudTestRunID(); runID != "" {
		return runID, nil
	}
	return cloudprovider.NewRunID()
}

func vastPrivateKey(secrets map[string]string) ([]byte, error) {
	value := strings.TrimSpace(secrets["VAST_SSH_PRIVATE_KEY"])
	if value == "" {
		value = strings.TrimSpace(secrets["VAST_SSH_KEY_PATH"])
	}
	if value == "" {
		return nil, nil
	}
	if strings.Contains(value, "BEGIN") {
		return []byte(value), nil
	}
	data, err := os.ReadFile(value)
	if err != nil {
		return nil, fmt.Errorf("read Vast SSH private key path %q: %w", value, err)
	}
	return data, nil
}

func TestCompshareZoneImagesMapsRegionsFromSecretNames(t *testing.T) {
	zoneImages := compshareZoneImages(map[string]string{
		"COMPSHARE_ZONE_IMAGE_CN_SH2": " compshareImage-sh2-golden ",
		"COMPSHARE_ZONE_IMAGE_UNSET":  "  ",
		"COMPSHARE_IMAGE_ID":          "compshareImage-base",
	})
	want := map[string]string{"cn-sh2": "compshareImage-sh2-golden"}
	if len(zoneImages) != len(want) {
		t.Fatalf("zone images = %v, want exactly %v", zoneImages, want)
	}
	for region, image := range want {
		if zoneImages[region] != image {
			t.Fatalf("zone image for %s = %q, want %q", region, zoneImages[region], image)
		}
	}
}

// CI passes per-region images as plain environment variables, not a secrets file.
func TestReadCloudSecretsPicksUpZoneImagesFromEnvironment(t *testing.T) {
	t.Setenv("COMPSHARE_ZONE_IMAGE_CN_SH2", " compshareImage-sh2-from-env ")
	zoneImages := compshareZoneImages(readCloudSecrets())
	if got := zoneImages["cn-sh2"]; got != "compshareImage-sh2-from-env" {
		t.Fatalf("zone image for cn-sh2 = %q, want the environment value (all: %v)", got, zoneImages)
	}
}
