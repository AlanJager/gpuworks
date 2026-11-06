package serverconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/securefile"
)

var (
	ErrOutboundPolicyConflict    = errors.New("outbound policy changed; reload before saving")
	ErrOutboundPolicyInvalid     = errors.New("invalid outbound policy")
	errOutboundPolicyUnavailable = errors.New("outbound policy configuration is unavailable")
	// Coordinate this setting with the other in-process YAML-key editors.
	configDocumentMu sync.Mutex
)

const (
	maxOutboundPolicyHosts     = 128
	maxOutboundPolicyHostBytes = 253
)

// NormalizeOutboundPrivateHosts validates exact hosts, IPs and CIDRs without
// DNS. Errors describe only the rule, never the supplied text or credentials.
func NormalizeOutboundPrivateHosts(values []string) ([]string, error) {
	if len(values) > maxOutboundPolicyHosts {
		return nil, fmt.Errorf("%w: at most %d host entries are allowed", ErrOutboundPolicyInvalid, maxOutboundPolicyHosts)
	}
	var result []string
	seen := make(map[string]bool)
	for i, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(value) > maxOutboundPolicyHostBytes {
			return nil, fmt.Errorf("%w: host entry %d is too long", ErrOutboundPolicyInvalid, i+1)
		}
		normalized, ok := normalizeOutboundHost(value)
		if !ok {
			return nil, fmt.Errorf("%w: host entry %d must be an exact ASCII host, IP or CIDR without a URL, port or wildcard", ErrOutboundPolicyInvalid, i+1)
		}
		if !seen[normalized] {
			seen[normalized] = true
			result = append(result, normalized)
		}
	}
	return result, nil
}

func normalizeOutboundHost(value string) (string, bool) {
	if prefix, err := netip.ParsePrefix(value); err == nil {
		if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		return prefix.Masked().String(), true
	}
	if address, err := netip.ParseAddr(value); err == nil {
		if address.Zone() != "" {
			return "", false
		}
		return address.Unmap().String(), true
	}
	host := strings.ToLower(strings.TrimSuffix(value, "."))
	if host == "" {
		return "", false
	}
	hasNonNumeric := false
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, ch := range label {
			if ch >= 'a' && ch <= 'z' || ch == '-' {
				hasNonNumeric = true
				continue
			}
			if ch < '0' || ch > '9' {
				return "", false
			}
		}
	}
	return host, hasNonNumeric
}

// ReadOutboundPolicy returns configured values, not the transport's expanded
// default-open exceptions. The revision hashes only these policy values.
func ReadOutboundPolicy(path string) (restrict bool, hosts []string, revision string, err error) {
	raw, _, err := readEditableOutboundSnapshot(path)
	if err != nil {
		return false, nil, "", err
	}
	cfg, _, err := decodeOutboundPolicyDocument(path, raw, false)
	if err != nil {
		return false, nil, "", err
	}
	return cfg.OutboundRestrictPrivateHosts, append([]string(nil), cfg.OutboundAllowPrivateHosts...), outboundPolicyRevision(cfg.OutboundRestrictPrivateHosts, cfg.OutboundAllowPrivateHosts), nil
}

// SaveOutboundPolicy edits only the two policy keys, preserving unrelated YAML
// values/comments and permissions. A stale revision never changes the file.
func SaveOutboundPolicy(path string, restrict bool, hosts []string, revision string) (string, error) {
	normalized, err := NormalizeOutboundPrivateHosts(hosts)
	if err != nil {
		return "", err
	}
	if revision == "" {
		return "", ErrOutboundPolicyConflict
	}
	configDocumentMu.Lock()
	defer configDocumentMu.Unlock()
	raw, info, err := readEditableOutboundSnapshot(path)
	if err != nil {
		return "", err
	}
	// Replacing a file must not discard its access ACL or activate grants
	// inherited by the temporary file when its group mode is restored.
	if err := securefile.CheckFileACL(path); err != nil {
		return "", errOutboundPolicyUnavailable
	}
	cfg, doc, err := decodeOutboundPolicyDocument(path, raw, true)
	if err != nil {
		return "", err
	}
	if outboundPolicyRevision(cfg.OutboundRestrictPrivateHosts, cfg.OutboundAllowPrivateHosts) != revision {
		return "", ErrOutboundPolicyConflict
	}
	flag := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"}
	if restrict {
		flag.Value = "true"
	}
	list := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, host := range normalized {
		list.Content = append(list.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: host})
	}
	setKey(doc.Content[0], "outbound_restrict_private_hosts", flag)
	setKey(doc.Content[0], "outbound_allow_private_hosts", list)
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(doc); err != nil {
		return "", ErrOutboundPolicyInvalid
	}
	if err := encoder.Close(); err != nil {
		return "", ErrOutboundPolicyInvalid
	}
	payload := buf.Bytes()
	if len(payload) > maxOutboundConfigBytes {
		return "", fmt.Errorf("%w: configuration exceeds the runtime size limit", ErrOutboundPolicyInvalid)
	}
	written, _, err := decodeOutboundPolicyDocument(path, payload, false)
	if err != nil || written.OutboundRestrictPrivateHosts != restrict || !slices.Equal(written.OutboundAllowPrivateHosts, normalized) {
		return "", ErrOutboundPolicyInvalid
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".outbound-policy-*")
	if err != nil {
		return "", errOutboundPolicyUnavailable
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := securefile.CheckFileACL(name); err != nil {
		_ = tmp.Close()
		return "", errOutboundPolicyUnavailable
	}

	if _, err = tmp.Write(payload); err != nil {
		tmp.Close()
		return "", errOutboundPolicyUnavailable
	}
	if err = tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return "", errOutboundPolicyUnavailable
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return "", errOutboundPolicyUnavailable
	}
	if err = tmp.Close(); err != nil {
		return "", errOutboundPolicyUnavailable
	}
	latest, latestInfo, err := readEditableOutboundSnapshot(path)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(raw, latest) || !sameOutboundConfigSnapshot(info, latestInfo) {
		return "", ErrOutboundPolicyConflict
	}
	if err = os.Rename(name, path); err != nil {
		return "", errOutboundPolicyUnavailable
	}
	return outboundPolicyRevision(restrict, normalized), nil
}

func readEditableOutboundSnapshot(path string) ([]byte, os.FileInfo, error) {
	raw, info, err := readOutboundSnapshot(path)
	if err != nil {
		if errors.Is(err, ErrOutboundPolicyInvalid) {
			return nil, nil, err
		}
		if errors.Is(err, errOutboundSnapshotChanged) {
			return nil, nil, ErrOutboundPolicyConflict
		}
		return nil, nil, errOutboundPolicyUnavailable
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() {
		return nil, nil, errOutboundPolicyUnavailable
	}
	if !sameOutboundConfigSnapshot(info, current) {
		return nil, nil, ErrOutboundPolicyConflict
	}
	return raw, info, nil
}

func decodeOutboundPolicyDocument(path string, raw []byte, writing bool) (File, *yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return File{}, nil, ErrOutboundPolicyInvalid
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return File{}, nil, ErrOutboundPolicyInvalid
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return File{}, nil, ErrOutboundPolicyInvalid
	}
	if writing && unsafeOutboundYAML(&doc) {
		return File{}, nil, fmt.Errorf("%w: aliased or merged configuration cannot be edited here", ErrOutboundPolicyInvalid)
	}
	cfg, err := decodeFile(path, raw)
	if err != nil {
		return File{}, nil, ErrOutboundPolicyInvalid
	}
	if _, err = NormalizeOutboundPrivateHosts(cfg.OutboundAllowPrivateHosts); err != nil {
		return File{}, nil, err
	}
	return cfg, &doc, nil
}

func unsafeOutboundYAML(node *yaml.Node) bool {
	if node.Kind == yaml.AliasNode || node.Tag == "!!merge" {
		return true
	}
	for _, child := range node.Content {
		if unsafeOutboundYAML(child) {
			return true
		}
	}
	return false
}

func outboundPolicyRevision(restrict bool, hosts []string) string {
	if hosts == nil {
		hosts = []string{}
	}
	raw, _ := json.Marshal([]any{restrict, hosts})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
