package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

func main() {
	listOnly := flag.Bool("list-only", false, "list leaked test instances without destroying them")
	details := flag.Bool("details", false, "show sanitized provider instance metadata")
	runID := flag.String("run-id", strings.TrimSpace(os.Getenv("GPUWORKS_CLOUD_TEST_RUN_ID")), "only clean resources owned by this test run")
	allRuns := flag.Bool("all", false, "clean all gpuworks test resources in the account (manual recovery only)")
	flag.Parse()
	if strings.TrimSpace(*runID) == "" && !*allRuns {
		fatal(fmt.Errorf("cleanup requires --run-id (or GPUWORKS_CLOUD_TEST_RUN_ID); use --all only for deliberate account-wide recovery"))
	}
	root := findRepoRoot()
	runpodKey := envOrFileAny(root, "RUNPOD_API_KEY", "RUNPOD_KEY")
	compshareAccess, compshareSecret := compshareCredentials(root)
	vastKey := envOrFile(root, "VAST_API_KEY")
	vastPrivateKey := readPrivateKey(envOrFileAny(root, "VAST_SSH_PRIVATE_KEY", "VAST_SSH_KEY_PATH"))
	if vastKey != "" && len(vastPrivateKey) == 0 {
		var err error
		vastPrivateKey, _, err = cloudprovider.GenerateSSHKey()
		if err != nil {
			fatal(fmt.Errorf("generate ephemeral Vast cleanup key: %w", err))
		}
	}
	autodlToken := envOrFile(root, "AUTODL_TOKEN")
	giteeToken := envOrFile(root, "GITEE_API_TOKEN")
	var providers []cloudprovider.CloudProvider
	if runpodKey != "" {
		p, err := cloudprovider.NewRunpod(runpodKey, "cleanup")
		if err != nil {
			fatal(err)
		}
		providers = append(providers, p)
	}
	if compshareAccess != "" && compshareSecret != "" {
		p, err := cloudprovider.NewCompshare(compshareAccess, compshareSecret, "cleanup", "cleanup")
		if err != nil {
			fatal(err)
		}
		providers = append(providers, p)
	}
	if vastKey != "" && len(vastPrivateKey) > 0 {
		p, err := cloudprovider.NewVast(vastKey, "cleanup", vastPrivateKey)
		if err != nil {
			fatal(err)
		}
		providers = append(providers, p)
	}
	if autodlToken != "" {
		p, err := cloudprovider.NewAutoDL(autodlToken, "cleanup")
		if err != nil {
			fatal(err)
		}
		providers = append(providers, p)
	}
	if giteeToken != "" {
		// Gitee has no server-side tag/name field, so ownership is tracked in
		// a local per-runID ledger (see cloudprovider.Gitee) and Destroy checks
		// the constructing provider's own runID against it. The CI cleanup
		// path always supplies --run-id, so scoped Destroy works correctly
		// there. Listing (ListLeaked/-list-only/--all) reads the whole ledger
		// regardless of this provider's own runID, so it stays accurate even
		// when no --run-id is given; only an unscoped --all Destroy attempt
		// against an instance owned by a different run would still fail with
		// an ownership error, same as manually targeting the wrong run-id.
		giteeRunID := strings.TrimSpace(*runID)
		if giteeRunID == "" {
			giteeRunID = "cleanup"
		}
		// zone/image are only consulted by Rent; a cleanup-only instance never
		// rents, so placeholder values satisfy the constructor without implying
		// a real compute target.
		p, err := cloudprovider.NewGitee(giteeToken, "cleanup", "cleanup", giteeRunID)
		if err != nil {
			fatal(err)
		}
		providers = append(providers, p)
	}
	if len(providers) == 0 {
		fmt.Fprintln(os.Stderr, "no cloud-provider credentials configured; no leaked instances were scanned")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, provider := range providers {
		if *details {
			if lister, ok := provider.(interface {
				ListInstances(context.Context) ([]serverprovider.Instance, error)
			}); ok {
				instances, err := lister.ListInstances(ctx)
				if err != nil {
					fatal(fmt.Errorf("list %s instance details: %w", provider.Name(), err))
				}
				for _, instance := range instances {
					if strings.HasPrefix(instance.Name, cloudprovider.TestNamePrefix) {
						fmt.Printf("%s instance id=%s name=%s state=%s region=%s public_ip=%s private_ip=%s ssh=%s:%d gpu=%s x%d kind=%s\n", provider.Name(), instance.ExternalID, instance.Name, instance.Power, instance.Region, instance.PublicIP, instance.PrivateIP, instance.SSHUser, instance.SSHPort, instance.GPUModel, instance.GPUCount, instance.Kind)
					}
				}
			}
		}
		var leaked []string
		var err error
		if *allRuns {
			err = retry(ctx, func() (err error) { leaked, err = provider.ListLeaked(ctx); return })
		} else if lister, ok := provider.(cloudprovider.ScopedLeakLister); ok {
			err = retry(ctx, func() (err error) { leaked, err = lister.ListLeakedForRunID(ctx, strings.TrimSpace(*runID)); return })
		} else {
			fmt.Printf("provider %s does not support run-scoped cleanup; skipped\n", provider.Name())
			continue
		}
		if err != nil {
			fatal(fmt.Errorf("list leaked %s instances: %w", provider.Name(), err))
		}
		for _, id := range leaked {
			if provider.Name() == "compshare" && *allRuns {
				fmt.Printf("protected compshare test instance %s (not destroyed; ownership may belong to another agent)\n", id)
				continue
			}
			if *listOnly {
				fmt.Printf("leaked %s instance %s\n", provider.Name(), id)
				continue
			}
			if err := retry(ctx, func() error { return provider.Destroy(ctx, id) }); err != nil {
				fatal(fmt.Errorf("destroy %s instance %s: %w", provider.Name(), id, err))
			}
			fmt.Printf("destroyed leaked %s instance %s\n", provider.Name(), id)
		}
		if len(leaked) == 0 {
			fmt.Printf("no leaked %s test instances found\n", provider.Name())
		}
	}
}

// retry runs call up to three times, a few seconds apart. The provider APIs
// are reached through a shared proxy that now and then drops a connection
// (an EOF listing RunPod pods failed a whole nightly job in its before_script
// seven seconds before the same call succeeded in after_script); one dropped
// connection must not fail the job or, in after_script, leave an instance
// running.
func retry(ctx context.Context, call func() error) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if err = call(); err == nil {
			return nil
		}
		if attempt == 3 {
			break
		}
		fmt.Fprintf(os.Stderr, "attempt %d failed, retrying: %v\n", attempt, err)
		select {
		case <-time.After(time.Duration(attempt) * 5 * time.Second):
		case <-ctx.Done():
			return err
		}
	}
	return err
}

func readPrivateKey(value string) []byte {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.Contains(value, "BEGIN") {
		return []byte(value)
	}
	data, err := os.ReadFile(value)
	if err != nil {
		return nil
	}
	return data
}

func envOrFile(root, key string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return readEnvFile(root, key)
}

func envOrFileAny(root string, keys ...string) string {
	for _, key := range keys {
		if value := envOrFile(root, key); value != "" {
			return value
		}
	}
	return ""
}

func compshareCredentials(root string) (string, string) {
	access := envOrFile(root, "COMPSHARE_ACCESS_KEY")
	secret := envOrFile(root, "COMPSHARE_SECRET_KEY")
	if access != "" && secret != "" {
		return access, secret
	}
	combined := envOrFile(root, "COMPSHARE_KEY")
	sep := "."
	if strings.Contains(combined, ":") {
		sep = ":"
	}
	parts := strings.SplitN(combined, sep, 2)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		if access == "" {
			access = parts[0]
		}
		if secret == "" {
			secret = parts[1]
		}
	}
	return access, secret
}

func findRepoRoot() string {
	dir, _ := os.Getwd()
	for dir != "" {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "."
}

func readEnvFile(root, key string) string {
	for _, filename := range []string{"cloud-providers.env", "cloud.env"} {
		file, err := os.Open(filepath.Join(root, "secrets", filename))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			name, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
			if ok && strings.TrimSpace(name) == key {
				_ = file.Close()
				return strings.Trim(strings.TrimSpace(value), "\"'")
			}
		}
		_ = file.Close()
	}
	return ""
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
