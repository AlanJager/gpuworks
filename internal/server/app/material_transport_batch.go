//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const materialBatchPath = materialReplicaPath + "/batch"
const materialBatchType = "application/vnd.gpuworks.material-batch-v1"
const materialBatchLegacyCount = 32
const maxMaterialBatchCount = 128
const maxMaterialBatchBytes int64 = 8 << 20
const maxMaterialBatchRequestBytes int64 = 16 << 10
const materialBatchTimeout = 8 * time.Second
const materialBatchLimitHeader = "X-Gpuworks-Material-Batch-Limit"

// A peer that answers 503 has not presented different bytes. Callers may
// retry that member; a metadata or digest mismatch stays a failed proof.
var errRemoteMaterialBatchUnavailable = errors.New("remote material batch source is unavailable")

var materialBatchMagic = []byte("GWMATB1\n")

type materialBatchItem struct {
	Digest string `json:"digest"`
	Bytes  int64  `json:"bytes"`
}

type materialBatchRequest struct {
	Files []materialBatchItem `json:"files"`
}

func materialBatchLength(files []materialBatchItem) (int64, error) {
	if len(files) == 0 || len(files) > maxMaterialBatchCount {
		return 0, errors.New("material batch count is invalid")
	}
	total := int64(len(materialBatchMagic) + 2 + len(files)*(64+8))
	bytes := int64(0)
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		if !validMaterialDigest(file.Digest) || file.Bytes <= 0 || file.Bytes > maxMaterialBatchBytes || seen[file.Digest] || bytes+file.Bytes > maxMaterialBatchBytes {
			return 0, errors.New("material batch entry is invalid")
		}
		seen[file.Digest] = true
		bytes += file.Bytes
	}
	return total + bytes, nil
}

// serveMaterialBatch streams current canonical bytes, not stored receipts.
// Authentication and one confirmed material grant are enforced by the route.
func serveMaterialBatch(w http.ResponseWriter, r *http.Request, receiver *materialReceiver) {
	if r.Header.Get("Content-Type") != "application/json" || strings.TrimSpace(r.Header.Get("Authorization")) != "" || r.ContentLength > maxMaterialBatchRequestBytes {
		http.Error(w, "invalid material batch request", http.StatusBadRequest)
		return
	}
	var request materialBatchRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMaterialBatchRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid material batch request", http.StatusBadRequest)
		return
	}
	length, err := materialBatchLength(request.Files)
	if err != nil {
		http.Error(w, "invalid material batch entries", http.StatusBadRequest)
		return
	}
	files := make([]*os.File, 0, len(request.Files))
	identities := make([]os.FileInfo, 0, len(request.Files))
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	for _, item := range request.Files {
		path := filepath.Join(receiver.root, item.Digest[:2], item.Digest)
		file, identity, err := openReceivedMaterial(path, item.Bytes)
		if err != nil {
			http.Error(w, "material batch source unavailable", http.StatusNotFound)
			return
		}
		files = append(files, file)
		identities = append(identities, identity)
	}
	w.Header().Set("Content-Type", materialBatchType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Gpuworks-Material-Member", strconv.FormatUint(receiver.memberID, 10))
	w.Header().Set(materialBatchLimitHeader, strconv.Itoa(maxMaterialBatchCount))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(materialBatchMagic); err != nil {
		return
	}
	var number [8]byte
	binary.BigEndian.PutUint16(number[:2], uint16(len(files)))
	if _, err := w.Write(number[:2]); err != nil {
		return
	}
	buffer := make([]byte, 128<<10)
	for i, item := range request.Files {
		if r.Context().Err() != nil {
			return
		}
		if _, err := io.WriteString(w, item.Digest); err != nil {
			return
		}
		binary.BigEndian.PutUint64(number[:], uint64(item.Bytes))
		if _, err := w.Write(number[:]); err != nil {
			return
		}
		count, err := io.CopyBuffer(w, materialContextReader{r.Context(), io.LimitReader(files[i], item.Bytes)}, buffer)
		if err != nil || count != item.Bytes {
			return
		}
		path := filepath.Join(receiver.root, item.Digest[:2], item.Digest)
		if checkReceivedMaterialIdentity(path, identities[i]) != nil {
			return
		}
	}
}

// verifyRemoteMaterialBatch consumes every byte from one authenticated peer
// response and independently checks each requested digest. A partial response
// is never proof of any member's complete batch.
func verifyRemoteMaterialBatch(ctx context.Context, client *http.Client, endpoint string, files []materialBatchItem, expectedMemberID uint64) error {
	_, err := verifyRemoteMaterialBatchLimit(ctx, client, endpoint, files, expectedMemberID)
	return err
}

// The optional limit is a scheduling hint for this peer invocation only. It
// becomes usable after the entire pinned, authenticated response was verified.
// Missing, duplicate or unknown declarations retain the legacy request size.
func verifyRemoteMaterialBatchLimit(ctx context.Context, client *http.Client, endpoint string, files []materialBatchItem, expectedMemberID uint64) (int, error) {
	length, err := materialBatchLength(files)
	if err != nil || client == nil || expectedMemberID == 0 {
		return materialBatchLegacyCount, errors.New("remote material batch requires bounded files and member")
	}
	origin, err := url.Parse(endpoint)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.RawPath != "" || (origin.Path != "" && origin.Path != "/") {
		return materialBatchLegacyCount, errors.New("remote material batch requires an exact HTTPS member origin")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.VerifyConnection == nil {
		return materialBatchLegacyCount, errors.New("remote material batch requires a pinned member TLS verifier")
	}
	raw, err := json.Marshal(materialBatchRequest{Files: files})
	if err != nil || len(raw) > int(maxMaterialBatchRequestBytes) {
		return materialBatchLegacyCount, errors.New("remote material batch request exceeds limit")
	}
	batchCtx, cancel := context.WithTimeout(ctx, materialBatchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(batchCtx, http.MethodPost, strings.TrimSuffix(endpoint, "/")+materialBatchPath, bytes.NewReader(raw))
	if err != nil {
		return materialBatchLegacyCount, err
	}
	request.Header.Set("Content-Type", "application/json")
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := noRedirect.Do(request)
	if err != nil {
		return materialBatchLegacyCount, err
	}
	defer response.Body.Close()
	if response.TLS == nil || len(response.TLS.PeerCertificates) == 0 || (len(response.TLS.VerifiedChains) == 0 && !transport.TLSClientConfig.InsecureSkipVerify) {
		return materialBatchLegacyCount, errors.New("remote material batch peer identity was not verified")
	}
	if response.StatusCode == http.StatusServiceUnavailable {
		return materialBatchLegacyCount, errRemoteMaterialBatchUnavailable
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != materialBatchType || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Gpuworks-Material-Member") != strconv.FormatUint(expectedMemberID, 10) || response.ContentLength != length {
		return materialBatchLegacyCount, fmt.Errorf("remote material batch response metadata differs: status=%d content_type_match=%t cache_control_match=%t member_match=%t content_length=%d expected_length=%d",
			response.StatusCode, response.Header.Get("Content-Type") == materialBatchType,
			response.Header.Get("Cache-Control") == "no-store",
			response.Header.Get("X-Gpuworks-Material-Member") == strconv.FormatUint(expectedMemberID, 10),
			response.ContentLength, length)
	}
	if err := verifyMaterialBatchBody(batchCtx, response.Body, files); err != nil {
		return materialBatchLegacyCount, err
	}
	if values := response.Header.Values(materialBatchLimitHeader); len(values) == 1 && values[0] == strconv.Itoa(maxMaterialBatchCount) {
		return maxMaterialBatchCount, nil
	}
	return materialBatchLegacyCount, nil
}

func verifyMaterialBatchBody(ctx context.Context, body io.Reader, files []materialBatchItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	magic := make([]byte, len(materialBatchMagic))
	if _, err := io.ReadFull(body, magic); err != nil {
		return fmt.Errorf("read remote material batch framing: %w", err)
	}
	if !bytes.Equal(magic, materialBatchMagic) {
		return errors.New("remote material batch framing differs")
	}
	var number [8]byte
	if _, err := io.ReadFull(body, number[:2]); err != nil {
		return fmt.Errorf("read remote material batch count: %w", err)
	}
	if int(binary.BigEndian.Uint16(number[:2])) != len(files) {
		return errors.New("remote material batch count differs")
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		var digest [64]byte
		if _, err := io.ReadFull(body, digest[:]); err != nil {
			return fmt.Errorf("read remote material batch digest frame: %w", err)
		}
		if string(digest[:]) != file.Digest {
			return errors.New("remote material batch digest frame differs")
		}
		if _, err := io.ReadFull(body, number[:]); err != nil {
			return fmt.Errorf("read remote material batch length frame: %w", err)
		}
		if binary.BigEndian.Uint64(number[:]) != uint64(file.Bytes) {
			return errors.New("remote material batch length frame differs")
		}
		hash := sha256.New()
		count, err := io.CopyN(hash, body, file.Bytes)
		if err != nil {
			return fmt.Errorf("read remote material batch bytes: %w", err)
		}
		if count != file.Bytes || hex.EncodeToString(hash.Sum(nil)) != file.Digest {
			return errors.New("remote material batch bytes are missing or differ")
		}
	}
	var extra [1]byte
	n, err := body.Read(extra[:])
	if err != nil && err != io.EOF {
		return fmt.Errorf("read remote material batch end: %w", err)
	}
	if n != 0 || err != io.EOF {
		return errors.New("remote material batch has extra bytes or incomplete transport")
	}
	return ctx.Err()
}
