package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"gpuworks.ai/gpuworks/internal/server/distribution"
)

// noticesCommand signs the publisher's notices (-in, the unsigned payload
// kept in the repository) into the envelope servers read (-out), or, with
// -source, reads the published file back from each source the way servers do
// and checks it is the one just signed (-expect, the signed file).
func noticesCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("notices", flag.ContinueOnError)
	var in, out, expect, keyID, keyEnv string
	var sources stringList
	fs.StringVar(&in, "in", "", "unsigned notices payload to sign")
	fs.StringVar(&out, "out", "", "signed notices file to write")
	fs.Var(&sources, "source", "source to read the published notices back from (repeatable)")
	fs.StringVar(&expect, "expect", "", "with -source: the signed file every source must serve")
	fs.StringVar(&keyID, "key-id", envOr("GPUWORKS_DIST_SIGNING_KEY_ID", "gpuworks-dist-1"), "signing key id")
	fs.StringVar(&keyEnv, "key-env", "GPUWORKS_DIST_SIGNING_KEY", "environment variable holding the base64 ed25519 seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(sources) > 0 {
		return verifyNotices(sources, expect, distribution.TrustedKeys(), stdout)
	}
	if in == "" || out == "" {
		return errors.New("-in and -out are required")
	}
	key, err := distribution.PrivateKeyFromSeed(os.Getenv(keyEnv))
	if err != nil {
		return fmt.Errorf("$%s: %w", keyEnv, err)
	}
	raw, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	envelope, n, err := signNotices(raw, keyID, key, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, envelope, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "signed %d notices into %s\n", len(n.Notices), out)
	return nil
}

// signNotices validates the payload as servers will, stamps it with the
// signing time and seals it.
func signNotices(raw []byte, keyID string, key ed25519.PrivateKey, now time.Time) ([]byte, distribution.Notices, error) {
	n, err := distribution.ParseNotices(raw)
	if err != nil {
		return nil, distribution.Notices{}, err
	}
	n.PublishedAt = now
	payload, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return nil, distribution.Notices{}, err
	}
	envelope, err := distribution.Seal(payload, keyID, key)
	return envelope, n, err
}

func verifyNotices(sources []string, expect string, keys distribution.Keys, stdout io.Writer) error {
	var want *distribution.Notices
	if expect != "" {
		raw, err := os.ReadFile(expect)
		if err != nil {
			return err
		}
		payload, _, err := distribution.Open(raw, keys)
		if err != nil {
			return fmt.Errorf("%s: %w", expect, err)
		}
		n, err := distribution.ParseNotices(payload)
		if err != nil {
			return fmt.Errorf("%s: %w", expect, err)
		}
		want = &n
	}
	for _, source := range sources {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		got, err := (&distribution.Client{Sources: []string{source}, Keys: keys}).FetchNotices(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("%s: %w", source, err)
		}
		if want != nil && !got.Notices.PublishedAt.Equal(want.PublishedAt) {
			return fmt.Errorf("%s serves notices published at %s, want %s", source,
				got.Notices.PublishedAt.Format(time.RFC3339), want.PublishedAt.Format(time.RFC3339))
		}
		fmt.Fprintf(stdout, "%s: %d notices, published %s\n", source, len(got.Notices.Notices), got.Notices.PublishedAt.Format(time.RFC3339))
	}
	return nil
}
