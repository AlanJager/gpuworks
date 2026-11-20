//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const materialReplicaPath = "/_cluster/material-replicas"
const materialManifestPath = "/_cluster/material-manifests"
const maxMaterialReplicaBytes int64 = 256 << 30

// materialReceipt is an acknowledgement from the receiving member's durable
// local path. It is not a Store.MaterialReplica row or a readiness decision.
type materialReceipt struct {
	Digest   string `json:"digest"`
	MemberID uint64 `json:"member_id"`
	Bytes    int64  `json:"bytes"`
}

type materialReceiver struct {
	root     string
	memberID uint64
	mu       sync.Mutex
	syncFile func(*os.File) error
	syncDir  func(string) error
	onChange func()
	log      *slog.Logger
}

type materialContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader materialContextReader) Read(p []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(p)
}

func newMaterialReceiver(root string, memberID uint64) (*materialReceiver, error) {
	if root == "" || memberID == 0 {
		return nil, errors.New("material receiver requires a durable root and member identity")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return nil, errors.New("material receiver root must already be a directory")
	}
	return &materialReceiver{root: root, memberID: memberID, syncFile: (*os.File).Sync, syncDir: syncMaterialDir}, nil
}

// recoverOrphanedMaterialTransfers runs before a receiver starts accepting
// requests, while its caller holds the data directory's runtime lock. A
// killed process cannot run receive's defer, so its temporary names must be
// removed without touching published digest files or other directories.
func recoverOrphanedMaterialTransfers(ctx context.Context, receiver *materialReceiver) error {
	if receiver == nil {
		return errors.New("material recovery requires a receiver")
	}
	root, err := os.OpenRoot(receiver.root)
	if err != nil {
		return err
	}
	defer root.Close()
	prefixes, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, prefix := range prefixes {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := prefix.Name()
		if len(name) != 2 || strings.Trim(name, "0123456789abcdef") != "" || !prefix.IsDir() {
			continue
		}
		entries, err := fs.ReadDir(root.FS(), name)
		if err != nil {
			return err
		}
		removed := false
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				if removed {
					return errors.Join(err, syncMaterialRootDir(root, name))
				}
				return err
			}
			if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), ".receiving-") {
				continue
			}
			if err := root.Remove(name + "/" + entry.Name()); err != nil {
				if removed {
					return errors.Join(err, syncMaterialRootDir(root, name))
				}
				return err
			}
			removed = true
		}
		if removed {
			if err := syncMaterialRootDir(root, name); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func syncMaterialRootDir(root *os.Root, name string) error {
	dir, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// materialReceiveError adds a fixed diagnostic stage without changing the
// caller-visible error text or errors.Is identity. It carries no material data.
type materialReceiveError struct {
	stage string
	cause error
}

func (err *materialReceiveError) Error() string { return err.cause.Error() }
func (err *materialReceiveError) Unwrap() error { return err.cause }

func (receiver *materialReceiver) receive(ctx context.Context, digest string, source io.Reader) (receipt materialReceipt, resultErr error) {
	stage := "validate"
	defer func() {
		if resultErr != nil {
			resultErr = &materialReceiveError{stage: stage, cause: resultErr}
		}
	}()
	if receiver == nil || !validMaterialDigest(digest) {
		return receipt, errors.New("invalid material receiver or digest")
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	stage = "prefix_prepare"
	parent := filepath.Join(receiver.root, digest[:2])
	if err := os.Mkdir(parent, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return receipt, err
	}
	if info, err := os.Lstat(parent); err != nil || !info.IsDir() {
		return receipt, errors.New("material digest directory is invalid")
	}
	// Persist a newly created prefix before acknowledging any child file.
	stage = "root_sync"
	if err := receiver.syncDir(receiver.root); err != nil {
		return receipt, err
	}
	stage = "temporary_create"
	temporary, err := os.CreateTemp(parent, ".receiving-")
	if err != nil {
		return receipt, err
	}
	defer os.Remove(temporary.Name())
	stage = "body"
	hash := sha256.New()
	count, copyErr := io.Copy(io.MultiWriter(temporary, hash), materialContextReader{ctx, io.LimitReader(source, maxMaterialReplicaBytes+1)})
	if copyErr != nil || count == 0 || count > maxMaterialReplicaBytes {
		_ = temporary.Close()
		if err := ctx.Err(); err != nil {
			return receipt, err
		}
		return receipt, errors.New("material transfer is incomplete or exceeds its limit")
	}
	if err := ctx.Err(); err != nil {
		_ = temporary.Close()
		return receipt, err
	}
	stage = "digest"
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		_ = temporary.Close()
		return receipt, errors.New("material transfer digest mismatch")
	}
	stage = "file_sync"
	if err := receiver.syncFile(temporary); err != nil {
		_ = temporary.Close()
		return receipt, err
	}
	stage = "file_close"
	if err := temporary.Close(); err != nil {
		return receipt, err
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	stage = "publish"
	final := filepath.Join(parent, digest)
	if err := os.Link(temporary.Name(), final); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return receipt, err
		}
		// Another transfer may already have published the same digest. Never
		// replace that inode, and do not acknowledge corrupt existing bytes.
		stage = "existing_file"
		if err := verifyMaterialFile(final, digest, count); err != nil {
			return receipt, err
		}
	} else if receiver.onChange != nil {
		// The final inode is visible now, including when a later fsync fails.
		receiver.onChange()
	}
	// The receipt follows a directory sync that includes only the final name.
	stage = "temporary_remove"
	if err := os.Remove(temporary.Name()); err != nil {
		return receipt, err
	}
	stage = "prefix_sync"
	if err := receiver.syncDir(parent); err != nil {
		return receipt, err
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	return materialReceipt{Digest: digest, MemberID: receiver.memberID, Bytes: count}, nil
}

func verifyMaterialFile(path, digest string, size int64) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return errors.New("published material is missing or differs")
	}
	got, err := hashFile(path)
	if err != nil || got != digest {
		return errors.New("published material digest differs")
	}
	return nil
}

// probeMaterialFile hashes the current inode without returning its bytes over
// the network. A size or digest from a prior receipt is not enough here.
func probeMaterialFile(ctx context.Context, path, digest string) (int64, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > maxMaterialReplicaBytes {
		return 0, errors.New("material probe source is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return 0, errors.New("material probe source changed before reading")
	}
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	var count int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			count += int64(n)
			if count > before.Size() {
				return 0, errors.New("material probe source grew while reading")
			}
			_, _ = hash.Write(buffer[:n])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, readErr
		}
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, after) || count != before.Size() || hex.EncodeToString(hash.Sum(nil)) != digest {
		return 0, errors.New("material probe source differs")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return count, nil
}

func syncMaterialDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func validMaterialDigest(digest string) bool {
	return len(digest) == 64 && strings.Trim(digest, "0123456789abcdef") == ""
}

// materialRefusalHeader names why an authenticated receiver answered 503
// without a verdict on the sender's material. materialRefusalAuthorization:
// it could not commit its own material grant in time.
// materialRefusalNotStarted: the member is still closed and has no receiver
// yet, as after a restart. materialRefusalInterrupted: its check was
// interrupted before it concluded, as by a database leadership change.
const (
	materialRefusalHeader        = "X-Gpuworks-Material-Refusal"
	materialRefusalAuthorization = "authorization-unavailable"
	materialRefusalNotStarted    = "receiver-not-started"
	materialRefusalInterrupted   = "verification-interrupted"
)

// materialReceiverStatus is an authenticated receiver's HTTP refusal. On its
// own it is never an unavailable peer; a 503 becomes one only when the
// receiver reports it reached no verdict (authorization unavailable, no
// receiver started yet, or an interrupted check) or with independent evidence
// that its database is partitioned from this member.
type materialReceiverStatus struct {
	status       int
	text         string
	unauthorized bool
}

func newMaterialReceiverStatus(response *http.Response, text string) *materialReceiverStatus {
	return &materialReceiverStatus{status: response.StatusCode, text: text,
		unauthorized: response.StatusCode == http.StatusServiceUnavailable && uninspectedMaterialRefusal(response.Header.Get(materialRefusalHeader))}
}

func (e *materialReceiverStatus) Error() string { return e.text }

// materialStatusError keeps a missing route distinct from a receiver that
// refused the bytes. HTTP 404 here is the enrollment router, not a dial failure.
func materialStatusError(response *http.Response, body []byte) error {
	status := response.StatusCode
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > 80 {
		text = text[:80]
	}
	if status == http.StatusNotFound {
		if text == "" {
			return errors.New("material route is not registered: HTTP 404")
		}
		return fmt.Errorf("material route is not registered: HTTP 404 %s", text)
	}
	return newMaterialReceiverStatus(response, fmt.Sprintf("material receiver rejected transfer: HTTP %d", status))
}

// sendMaterialReplica transfers real source bytes to a pinned, authenticated
// target member. The caller supplies an mTLS HTTP client bound to that peer.
// It returns only a durable-receiver receipt; no replicated DB row is written.
func sendMaterialReplica(ctx context.Context, client *http.Client, endpoint, source, digest string, expectedMemberID uint64) (materialReceipt, error) {
	var receipt materialReceipt
	if client == nil || source == "" || !validMaterialDigest(digest) || expectedMemberID == 0 {
		return receipt, errors.New("material transfer requires a pinned HTTPS member and source")
	}
	origin, err := url.Parse(endpoint)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.RawPath != "" || (origin.Path != "" && origin.Path != "/") {
		return receipt, errors.New("material transfer requires an exact HTTPS member origin")
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxMaterialReplicaBytes {
		return receipt, errors.New("material transfer source is unavailable or exceeds its limit")
	}
	size, err := probeMaterialFile(ctx, source, digest)
	if err != nil {
		return receipt, err
	}
	if size != info.Size() {
		return receipt, errors.New("material transfer source changed or differs")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.VerifyConnection == nil {
		return receipt, errors.New("material transfer requires a pinned member TLS verifier")
	}
	file, err := os.Open(source)
	if err != nil {
		return receipt, err
	}
	defer file.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(endpoint, "/")+materialReplicaPath+"/"+digest, file)
	if err != nil {
		return receipt, err
	}
	request.ContentLength = info.Size()
	request.Header.Set("Content-Type", "application/octet-stream")
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := noRedirect.Do(request)
	if err != nil {
		return receipt, err
	}
	defer response.Body.Close()
	if response.TLS == nil || len(response.TLS.PeerCertificates) == 0 || (len(response.TLS.VerifiedChains) == 0 && !transport.TLSClientConfig.InsecureSkipVerify) {
		return receipt, errors.New("material receiver identity was not verified")
	}
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 128))
		return receipt, materialStatusError(response, body)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(raw) > 4096 {
		return receipt, errors.New("material receipt exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return materialReceipt{}, errors.New("invalid material receipt")
	}
	if err := decoder.Decode(new(any)); err != io.EOF || receipt.Digest != digest || receipt.MemberID != expectedMemberID || receipt.Bytes != info.Size() {
		return materialReceipt{}, errors.New("material receipt does not match the requested member or bytes")
	}
	return receipt, nil
}

// verifyRemoteMaterialReplica reads and hashes the peer's current bytes. A
// stored transfer receipt alone cannot prove the remote member still has them.
func verifyRemoteMaterialReplica(ctx context.Context, client *http.Client, endpoint, digest string, size int64, expectedMemberID uint64) error {
	if client == nil || !validMaterialDigest(digest) || size <= 0 || size > maxMaterialReplicaBytes || expectedMemberID == 0 {
		return errors.New("remote material probe requires a member, digest and size")
	}
	origin, err := url.Parse(endpoint)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.RawPath != "" || (origin.Path != "" && origin.Path != "/") {
		return errors.New("remote material probe requires an exact HTTPS member origin")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.VerifyConnection == nil {
		return errors.New("remote material probe requires a pinned member TLS verifier")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(endpoint, "/")+materialReplicaPath+"/"+digest, nil)
	if err != nil {
		return err
	}
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := noRedirect.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.TLS == nil || len(response.TLS.PeerCertificates) == 0 || (len(response.TLS.VerifiedChains) == 0 && !transport.TLSClientConfig.InsecureSkipVerify) {
		return errors.New("remote material peer identity was not verified")
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Gpuworks-Material-Member") != fmt.Sprint(expectedMemberID) || response.ContentLength != size {
		return errors.New("remote material member or length does not match")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(response.Body, size+1))
	if err != nil {
		return fmt.Errorf("read remote material bytes: %w", err)
	}
	if count != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("remote material bytes are missing or differ")
	}
	return nil
}

// probeRemoteMaterialReplica asks the pinned member to hash its current file.
// The HEAD response carries no material bytes, but is valid only when the
// receiver itself has checked the content before sending its headers.
func probeRemoteMaterialReplica(ctx context.Context, client *http.Client, endpoint, digest string, size int64, expectedMemberID uint64) error {
	if client == nil || !validMaterialDigest(digest) || size <= 0 || size > maxMaterialReplicaBytes || expectedMemberID == 0 {
		return errors.New("remote material probe requires a member, digest and size")
	}
	origin, err := url.Parse(endpoint)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.RawPath != "" || (origin.Path != "" && origin.Path != "/") {
		return errors.New("remote material probe requires an exact HTTPS member origin")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.VerifyConnection == nil {
		return errors.New("remote material probe requires a pinned member TLS verifier")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, strings.TrimSuffix(endpoint, "/")+materialReplicaPath+"/"+digest, nil)
	if err != nil {
		return err
	}
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := noRedirect.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.TLS == nil || len(response.TLS.PeerCertificates) == 0 || (len(response.TLS.VerifiedChains) == 0 && !transport.TLSClientConfig.InsecureSkipVerify) {
		return errors.New("remote material peer identity was not verified")
	}
	if response.StatusCode != http.StatusOK {
		// Keep the receiver's own account of a refusal: one it labels as made
		// before reading any byte is an unavailable peer, not missing bytes.
		return newMaterialReceiverStatus(response, fmt.Sprintf("remote material probe did not confirm current bytes: HTTP %d", response.StatusCode))
	}
	if response.Header.Get("X-Gpuworks-Material-Member") != fmt.Sprint(expectedMemberID) || response.Header.Get("X-Gpuworks-Material-Digest") != digest || response.Header.Get("Cache-Control") != "no-store" || response.ContentLength != size {
		return errors.New("remote material probe did not confirm current bytes")
	}
	return nil
}

// uninspectedMaterialRefusal reports the labels a receiver sends only when it
// refused without a verdict on the material.
func uninspectedMaterialRefusal(label string) bool {
	return label == materialRefusalAuthorization || label == materialRefusalNotStarted || label == materialRefusalInterrupted
}
