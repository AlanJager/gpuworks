// Package cloudprovider contains the lifecycle adapters used by L4 cloud-GPU tests.
package cloudprovider

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
)

const TestNamePrefix = "gpuworks-test-"

// runpodGPUAllowlist is deliberately small: cloud acceptance tests must not
// fall back to an unexpectedly expensive GPU class when inventory changes.
var runpodGPUAllowlist = []string{
	"NVIDIA RTX 2000 Ada Generation",
	"NVIDIA L4",
	"NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb",
	"NVIDIA RTX PRO 4000 Blackwell",
	"NVIDIA RTX A5000",
	"NVIDIA A40",
	"NVIDIA RTX A6000",
}

type CloudProvider interface {
	Name() string
	Rent(context.Context, GPUSpec) (*Instance, error)
	Destroy(context.Context, string) error
	ListLeaked(context.Context) ([]string, error)
}

// ScopedLeakLister restricts cleanup to resources owned by one test run. CI
// must use this interface so concurrent agents sharing a cloud account cannot
// delete one another's resources.
type ScopedLeakLister interface {
	ListLeakedForRunID(context.Context, string) ([]string, error)
}

func leakedInstanceIDsForRun(providerName, runID string, instances []serverprovider.Instance) ([]string, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, fmt.Errorf("%s: cleanup run ID is required", providerName)
	}
	wantName := TestNamePrefix + runID
	var leaked []string
	for _, instance := range instances {
		if instance.Name == wantName {
			leaked = append(leaked, instance.ExternalID)
		}
	}
	return leaked, nil
}

// ManagementProvider exposes an in-memory credential copy so L4 tests can
// configure the real gpuworks provider API after renting an instance. Callers
// must not log or persist the returned secret material outside the test server.
type ManagementProvider interface {
	ManagementCredentials() serverprovider.Credentials
}

type GPUSpec struct {
	GPUModel  string
	GPUCount  int
	MinVRAMMB int
	DiskGB    int
	// Region and Zone optionally pin the placement (e.g. building a golden
	// image in the provider's second region); empty lets the provider choose.
	Region string
	Zone   string
	// ServerDialPort asks for an instance whose provider exposes a port for a
	// server-dialed agent from the start, as the platform's own instances do.
	// RunPod only opens a port by editing the pod, which restarts it.
	ServerDialPort bool
}

type Instance struct {
	InstanceID   string
	GPUModel     string
	GPUCount     int
	Kind         string
	Region       string
	PublicIP     string
	PrivateIP    string
	SSHPort      int
	Username     string
	PrivateKey   []byte
	Password     string
	DataDiskPath string // optional: writable data disk (e.g. /data) to prefer for agent state
}

func NewRunID() (string, error) {
	var data [12]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate test run id: %w", err)
	}
	return hex.EncodeToString(data[:]), nil
}

func GenerateSSHKey() (privatePEM, publicAuthorizedKey []byte, err error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate SSH key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal SSH private key: %w", err)
	}
	sshPublicKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal SSH public key: %w", err)
	}
	privatePEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if len(privatePEM) == 0 {
		return nil, nil, errors.New("encode SSH private key")
	}
	return privatePEM, ssh.MarshalAuthorizedKey(sshPublicKey), nil
}

func NormalizeGPUModel(model string) string {
	model = strings.TrimSpace(model)
	switch strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "_", "").Replace(model)) {
	case "RTX4090", "4090":
		return "NVIDIA GeForce RTX 4090"
	case "A40", "NVIDIAA40":
		return "NVIDIA A40"
	case "L4", "NVIDIAL4":
		return "NVIDIA L4"
	case "L40", "NVIDIAL40":
		return "NVIDIA L40"
	case "L40S", "NVIDIAL40S":
		return "NVIDIA L40S"
	case "RTX2000ADA", "NVIDIARTX2000ADAGENERATION":
		return "NVIDIA RTX 2000 Ada Generation"
	case "PRO6000MIG24GB", "NVIDIAPRO6000MIG24GB", "RTXPRO6000BLACKWELLMIG24GB", "NVIDIARTXPRO6000BLACKWELLMIG24GB":
		return "NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb"
	case "RTXPRO6000BLACKWELLSERVEREDITIONMIG1G.24GB", "NVIDIARTXPRO6000BLACKWELLSERVEREDITIONMIG1G.24GB":
		return "NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb"
	case "RTXPRO4000", "NVIDIARTXPRO4000":
		return "NVIDIA RTX PRO 4000 Blackwell"
	case "RTXA5000", "NVIDIARTXA5000":
		return "NVIDIA RTX A5000"
	case "RTXA6000", "NVIDIARTXA6000":
		return "NVIDIA RTX A6000"
	case "RTX4000ADA", "NVIDIARTX4000ADAGENERATION":
		return "NVIDIA RTX 4000 Ada Generation"
	case "A100-40GB", "A10040GB":
		return "NVIDIA A100 40GB PCIe"
	case "A100-80GB", "A10080GB":
		return "NVIDIA A100 80GB PCIe"
	default:
		return model
	}
}
