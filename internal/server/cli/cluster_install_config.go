package cli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// SSH settings belong to the management machine; they never enter node requests.
type clusterInstallSSH struct {
	Target string `yaml:"target,omitempty"`
	User   string `yaml:"user,omitempty"`
	Key    string `yaml:"key,omitempty"`
	Port   int    `yaml:"port,omitempty"`
}

type clusterInstallNode struct {
	Name    string            `yaml:"name"`
	Address string            `yaml:"address"`
	Dir     string            `yaml:"dir"`
	SSH     clusterInstallSSH `yaml:"ssh"`
}

type clusterInstallConfig struct {
	Version  int `yaml:"version"`
	Defaults struct {
		Dir string            `yaml:"dir"`
		SSH clusterInstallSSH `yaml:"ssh"`
	} `yaml:"defaults,omitempty"`
	Ports struct {
		HTTP       int `yaml:"http"`
		Enrollment int `yaml:"enrollment"`
		Database   int `yaml:"database"`
	} `yaml:"ports"`
	// StorageTier is standard or slow-storage; absent lets the installation
	// choose it from the storage measurement.
	StorageTier string               `yaml:"storage_tier,omitempty"`
	Nodes       []clusterInstallNode `yaml:"nodes"`
}

var clusterInstallUserPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*[$]?$`)
var clusterInstallTargetPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func (o clusterInstallOptions) node(host string) clusterInstallNode {
	for _, node := range o.Nodes {
		if node.Address == host {
			return node
		}
	}
	return clusterInstallNode{Name: host, Address: host, Dir: o.Dir, SSH: clusterInstallSSH{Target: host, User: o.User, Key: o.Key}}
}

func readClusterInstallConfig(path string, o *clusterInstallOptions, explicit map[string]bool) error {
	for _, flag := range []string{"hosts", "ssh-user", "ssh-key", "dir"} {
		if explicit[flag] {
			return fmt.Errorf("--config cannot be combined with --%s; use defaults or node settings in the config", flag)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return errors.New("cluster config must be a regular file of at most 64 KiB")
	}
	decoder := yaml.NewDecoder(io.LimitReader(f, 65537))
	decoder.KnownFields(true)
	var config clusterInstallConfig
	if err := decoder.Decode(&config); err != nil {
		return errors.New("invalid cluster config: check YAML fields, types and duplicate keys")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("cluster config must contain exactly one YAML document")
	}
	if config.Version != 1 {
		return errors.New("cluster config version must be 1")
	}
	if len(config.Nodes) != 3 {
		return errors.New("cluster config requires exactly three control-plane nodes")
	}
	if config.StorageTier != "" {
		tier, err := storagetier.Parse(config.StorageTier)
		if err != nil {
			return fmt.Errorf("cluster config storage_tier: %w", err)
		}
		o.StorageTier = tier
	}
	if config.Defaults.SSH.Target != "" {
		return errors.New("ssh.target is a per-node setting")
	}
	if err := validateClusterInstallSSH(config.Defaults.SSH); err != nil {
		return fmt.Errorf("defaults: %w", err)
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for i, node := range config.Nodes {
		if node.Name == "" {
			node.Name = fmt.Sprintf("control-%d", i+1)
		}
		if !clusterInstallTargetPattern.MatchString(node.Name) || names[node.Name] {
			return errors.New("node names must be distinct and contain only letters, digits, dots, underscores and hyphens")
		}
		names[node.Name] = true
		if node.Dir == "" {
			node.Dir = config.Defaults.Dir
		}
		if node.Dir == "" && !o.NoService {
			node.Dir = clusterInstallDefaultDir("", false)
		}
		if node.Dir != "" {
			if err := validateClusterInstallDir(node.Dir); err != nil {
				return fmt.Errorf("%s: %w", node.Name, err)
			}
		}
		if node.SSH.Target == "" {
			node.SSH.Target = node.Address
		}
		if node.SSH.User == "" {
			node.SSH.User = config.Defaults.SSH.User
		}
		if node.SSH.Key == "" {
			node.SSH.Key = config.Defaults.SSH.Key
		}
		if node.SSH.Port == 0 {
			node.SSH.Port = config.Defaults.SSH.Port
		}
		if err := validateClusterInstallSSH(node.SSH); err != nil {
			return fmt.Errorf("%s: %w", node.Name, err)
		}
		if node.SSH.Key != "" && !filepath.IsAbs(node.SSH.Key) {
			node.SSH.Key = filepath.Join(base, node.SSH.Key)
		}
		o.Hosts = append(o.Hosts, node.Address)
		o.Nodes = append(o.Nodes, node)
	}
	for _, port := range []struct {
		name  string
		value int
		out   *int
	}{{"http-port", config.Ports.HTTP, &o.HTTP}, {"enrollment-port", config.Ports.Enrollment, &o.Enrollment}, {"database-port", config.Ports.Database, &o.Database}} {
		if port.value != 0 && !explicit[port.name] {
			*port.out = port.value
		}
	}
	return nil
}

func validateClusterInstallSSH(ssh clusterInstallSSH) error {
	if ssh.User != "" && !clusterInstallUserPattern.MatchString(ssh.User) {
		return errors.New("ssh.user requires a Unix account name")
	}
	if ssh.Target != "" && !clusterInstallTargetPattern.MatchString(ssh.Target) {
		return errors.New("ssh.target requires an IPv4 address, hostname or SSH alias (set the user separately)")
	}
	if ssh.Port < 0 || ssh.Port > 65535 {
		return errors.New("ssh.port must be between 1 and 65535 when specified")
	}
	if strings.ContainsAny(ssh.Key, "\x00\r\n") {
		return errors.New("ssh.key contains invalid characters")
	}
	return nil
}

func validateClusterInstallHosts(o *clusterInstallOptions) error {
	seen := map[string]bool{}
	for i, host := range o.Hosts {
		ip := net.ParseIP(strings.TrimSpace(host))
		if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() {
			return errors.New("cluster addresses require three reachable IPv4 addresses")
		}
		host = ip.String()
		if seen[host] {
			return errors.New("cluster addresses contain duplicate addresses")
		}
		seen[host] = true
		o.Hosts[i] = host
		if len(o.Nodes) != 0 {
			o.Nodes[i].Address = host
		}
	}
	if len(o.Hosts) != 3 {
		return errors.New("exactly three distinct IPv4 cluster addresses are required")
	}
	return nil
}

// Member sessions use the next database port; it is derived by the runtime.
func validateClusterInstallPorts(http, enrollment, database int) error {
	ports := map[int]bool{}
	for _, port := range []int{http, enrollment, database, database + 1} {
		if port < 1024 || port > 65535 || ports[port] {
			return errors.New("HTTP, enrollment, database and derived member-session (database+1) ports must be distinct and between 1024 and 65535")
		}
		ports[port] = true
	}
	return nil
}
