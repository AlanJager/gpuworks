package cluster

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
)

func TestDatabaseConfigRejectsUnprotectedTransport(t *testing.T) {
	base := DatabaseConfig{Dir: t.TempDir(), Address: "127.0.0.1:19000", Name: "gpuworks.db"}
	if err := base.validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*DatabaseConfig)
	}{
		{"public-listener", func(c *DatabaseConfig) { c.Address = "192.0.2.1:19000" }},
		{"public-seed", func(c *DatabaseConfig) { c.Seeds = []string{"192.0.2.1:19000"} }},
		{"wildcard", func(c *DatabaseConfig) { c.Address = ":19000" }},
		{"ephemeral-address", func(c *DatabaseConfig) { c.Address = "127.0.0.1:0" }},
		{"unsafe-name", func(c *DatabaseConfig) { c.Name = "../gpuworks.db" }},
		{"half-tls", func(c *DatabaseConfig) { c.DialTLS = &tls.Config{MinVersion: tls.VersionTLS13} }},
		{"unverified-tls", func(c *DatabaseConfig) {
			c.DialTLS = &tls.Config{InsecureSkipVerify: true}
			c.ListenTLS = &tls.Config{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.change(&cfg)
			if err := cfg.validate(); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestDatabaseConfigRejectsUnspecifiedTLSAddress(t *testing.T) {
	for _, address := range []string{"0.0.0.0:19000", "[::]:19000", "224.0.0.1:19000"} {
		cfg := DatabaseConfig{Dir: t.TempDir(), Address: address, Name: "gpuworks.db",
			ListenTLS: &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: x509.NewCertPool()},
			DialTLS:   &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: x509.NewCertPool()},
		}
		if err := cfg.validate(); err == nil {
			t.Fatalf("accepted non-member address %s", address)
		}
	}
}
