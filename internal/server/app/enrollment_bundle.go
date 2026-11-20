//go:build linux && dqlite

package app

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const enrollmentDirectory = "cluster-enrollment"

type enrollmentBundle struct {
	info    EnrollmentInfo
	dataDir string
	issuer  *identity.Authority
	tls     *tls.Config
	master  string
	trust   *identity.Trust
	ca      []byte
	caKey   []byte
}

func validateEnrollmentAddresses(listen, origin string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", errors.New("enrollment listen address requires host and port")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", errors.New("enrollment listen port must be between 1 and 65535")
	}
	if host != "" && net.ParseIP(host) == nil {
		return "", errors.New("enrollment listener requires a literal local IP or wildcard host")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("enrollment public URL must be an HTTPS origin without credentials, path or query")
	}
	publicPort := u.Port()
	if publicPort == "" {
		publicPort = "443"
	}
	p, err = strconv.Atoi(publicPort)
	if err != nil || p < 1 || p > 65535 {
		return "", errors.New("invalid enrollment public port")
	}
	return net.JoinHostPort(u.Hostname(), publicPort), nil
}

func readEnrollmentBundle(dataDir string, allowPending bool) (*enrollmentBundle, error) {
	dir := filepath.Join(dataDir, enrollmentDirectory)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := enrollmentPlatform(); err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("enrollment material directory must be private and cannot be a link")
	}
	if _, err := os.Lstat(filepath.Join(dir, ".pending")); err == nil && !allowPending {
		return nil, errors.New("cluster enrollment initialization is incomplete; repeat the same offline cluster-init operation")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	read := func(name string) ([]byte, error) {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() <= 0 || info.Size() > 128<<10 {
			return nil, errors.New("invalid or exposed enrollment material")
		}
		return os.ReadFile(path)
	}
	config, err := read("config.json")
	if err != nil {
		return nil, err
	}
	var settings EnrollmentInfo
	decoder := json.NewDecoder(bytes.NewReader(config))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return nil, errors.New("invalid enrollment configuration")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid trailing enrollment configuration")
	}
	if settings.Protocol != store.ClusterProtocolVersion {
		return nil, errors.New("unsupported enrollment protocol")
	}
	if tier, err := storagetier.Parse(settings.StorageTier); err != nil || tier.Recorded() != settings.StorageTier {
		return nil, errors.New("unsupported enrollment storage tier")
	}
	publicAddress, err := validateEnrollmentAddresses(settings.ListenAddress, settings.PublicURL)
	if err != nil {
		return nil, err
	}
	ca, err := read("ca.pem")
	if err != nil {
		return nil, err
	}
	caKey, err := read("ca.key")
	if err != nil {
		return nil, err
	}
	master, err := enrollmentMasterSecret(dataDir)
	if err != nil {
		return nil, err
	}
	binding, err := read("master.binding")
	if err != nil {
		return nil, err
	}
	if !hmac.Equal(binding, enrollmentMasterBinding(caKey, master)) {
		return nil, errors.New("effective master secret differs from enrollment initialization; restore the original secret")
	}
	now := time.Now().UTC()
	issuer, err := identity.LoadAuthority(ca, caKey, settings.ClusterID, settings.CAFingerprint, now)
	if err != nil {
		return nil, err
	}
	trust, err := identity.NewTrust(ca, settings.ClusterID, settings.CAFingerprint, now)
	if err != nil {
		return nil, err
	}
	certificate, err := read("server.pem")
	if err != nil {
		return nil, err
	}
	key, err := read("server.key")
	if err != nil {
		return nil, err
	}
	// The chain includes the public root so the joining client can validate
	// the preconfigured CA pin before sending its invitation over this TLS link.
	pair, err := tls.X509KeyPair(append(append([]byte{}, certificate...), ca...), key)
	if err != nil {
		return nil, errors.New("enrollment server certificate and private key mismatch")
	}
	pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	if _, err := trust.VerifyMember(pair.Leaf, now); err != nil {
		return nil, err
	}
	publicHost, _, _ := net.SplitHostPort(publicAddress)
	if err := pair.Leaf.VerifyHostname(publicHost); err != nil {
		return nil, errors.New("enrollment certificate does not cover its public URL")
	}
	tlsConfig, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, ca)
	if err != nil {
		return nil, err
	}
	return &enrollmentBundle{info: settings, dataDir: dataDir, issuer: issuer, master: string(master), tls: tlsConfig, trust: trust, ca: ca, caKey: caKey}, nil
}

// enrollmentAppSecret is the credential-encryption secret of a starting
// server. A loaded bundle already verified its secret against the cluster
// authority, so that snapshot is used and the file is not read again; a
// replaced or missing file cannot change the key after verification.
func enrollmentAppSecret(cfg Config, bundle *enrollmentBundle, policies ...securefile.Policy) (string, error) {
	if bundle != nil {
		return bundle.master, nil
	}
	return masterSecret(cfg, policies...)
}

// enrollmentMasterSecret reads the secret the application already uses. It
// never creates one: a replacement would not decrypt stored credentials. The
// server reads its secret from the data directory only, so an environment
// value that names a different secret is refused instead of silently ignored.
func enrollmentMasterSecret(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, "master.key")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64<<10 {
		return nil, errors.New("existing master secret is required; enrollment will not generate a replacement")
	}
	master, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if configured := os.Getenv("GPUWORKS_MASTER_SECRET"); configured != "" && !bytes.Equal([]byte(configured), master) {
		return nil, errors.New("configured master secret differs from the data directory; remove the environment value or restore the original secret")
	}
	return master, nil
}

// Bind the effective secret without exposing it (or a public password digest)
// in EnrollmentInfo. The tag is meaningful only with the private CA material.
func enrollmentMasterBinding(caKey, master []byte) []byte {
	mac := hmac.New(sha256.New, caKey)
	_, _ = mac.Write(master)
	return []byte(hex.EncodeToString(mac.Sum(nil)))
}
