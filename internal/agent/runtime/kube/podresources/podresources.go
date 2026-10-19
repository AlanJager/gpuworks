// Package podresources asks the kubelet which devices it gave to which pod:
// the List call of the kubelet's PodResources API (k8s.io/kubelet
// pkg/apis/podresources/v1), over its Unix socket. It speaks just enough
// gRPC — HTTP/2 without TLS, a length-prefixed request and response, the
// status in the trailers — and decodes the few protobuf fields it needs by
// hand, so the agent carries neither a gRPC nor a protobuf library for one
// call.
package podresources

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DefaultSocket is the kubelet's PodResources socket.
const DefaultSocket = "/var/lib/kubelet/pod-resources/kubelet.sock"

const listPath = "/v1.PodResourcesLister/List"

// Allocation is one device the kubelet gave a pod's container.
type Allocation struct {
	Namespace string
	Pod       string
	Container string
	Resource  string // e.g. nvidia.com/gpu
	DeviceID  string // e.g. GPU-8a0f…, or a device plugin's own ID
}

// Client calls one kubelet.
type Client struct {
	socket string
	http   *http.Client
}

// New returns a client for the kubelet socket at path.
func New(path string) *Client {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: &protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
	return &Client{socket: path, http: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
}

// List returns every device the kubelet has allocated, by pod and container.
func (c *Client) List(ctx context.Context) ([]Allocation, error) {
	// An empty ListPodResourcesRequest, gRPC framed: not compressed, length 0.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://kubelet"+listPath, bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kubelet pod resources %s: %w", c.socket, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kubelet pod resources: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if st := grpcStatus(resp); st != "" && st != "0" {
		return nil, fmt.Errorf("kubelet pod resources: grpc-status %s: %s", st, firstNonEmpty(resp.Trailer.Get("Grpc-Message"), resp.Header.Get("Grpc-Message")))
	}
	if len(body) < 5 {
		return nil, errors.New("kubelet pod resources: short response")
	}
	if body[0] != 0 {
		return nil, errors.New("kubelet pod resources: compressed responses are not supported")
	}
	n := binary.BigEndian.Uint32(body[1:5])
	if int(n) != len(body)-5 {
		return nil, fmt.Errorf("kubelet pod resources: frame says %d bytes, got %d", n, len(body)-5)
	}
	return decodeList(body[5:])
}

func grpcStatus(resp *http.Response) string {
	if st := resp.Trailer.Get("Grpc-Status"); st != "" {
		return st
	}
	return resp.Header.Get("Grpc-Status") // a trailers-only answer
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// decodeList reads ListPodResourcesResponse{repeated PodResources
// pod_resources = 1}.
func decodeList(b []byte) ([]Allocation, error) {
	var out []Allocation
	err := fields(b, func(num int, payload []byte) error {
		if num != 1 {
			return nil
		}
		return decodePod(payload, &out)
	})
	return out, err
}

// decodePod reads PodResources{string name = 1; string namespace = 2;
// repeated ContainerResources containers = 3}.
func decodePod(b []byte, out *[]Allocation) error {
	var name, ns string
	var containers [][]byte
	if err := fields(b, func(num int, payload []byte) error {
		switch num {
		case 1:
			name = string(payload)
		case 2:
			ns = string(payload)
		case 3:
			containers = append(containers, payload)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, c := range containers {
		var cname string
		var devices [][]byte
		if err := fields(c, func(num int, payload []byte) error {
			switch num {
			case 1:
				cname = string(payload)
			case 2:
				devices = append(devices, payload)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, d := range devices {
			var resource string
			var ids []string
			if err := fields(d, func(num int, payload []byte) error {
				switch num {
				case 1:
					resource = string(payload)
				case 2:
					ids = append(ids, string(payload))
				}
				return nil
			}); err != nil {
				return err
			}
			for _, id := range ids {
				*out = append(*out, Allocation{Namespace: ns, Pod: name, Container: cname, Resource: resource, DeviceID: id})
			}
		}
	}
	return nil
}

// fields walks a protobuf message, handing every length-delimited field to
// fn and skipping the other wire types.
func fields(b []byte, fn func(num int, payload []byte) error) error {
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return errors.New("protobuf: bad field key")
		}
		b = b[n:]
		num, wire := int(key>>3), key&7
		switch wire {
		case 0: // varint
			_, n := binary.Uvarint(b)
			if n <= 0 {
				return errors.New("protobuf: bad varint")
			}
			b = b[n:]
		case 1: // 64-bit
			if len(b) < 8 {
				return errors.New("protobuf: short fixed64")
			}
			b = b[8:]
		case 5: // 32-bit
			if len(b) < 4 {
				return errors.New("protobuf: short fixed32")
			}
			b = b[4:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return errors.New("protobuf: bad length")
			}
			payload := b[n : n+int(l)]
			b = b[n+int(l):]
			if err := fn(num, payload); err != nil {
				return err
			}
		default:
			return fmt.Errorf("protobuf: unsupported wire type %d", wire)
		}
	}
	return nil
}

// IsGPUResource reports whether a device plugin resource names accelerator
// cards (nvidia.com/gpu, huawei.com/Ascend910…): the vendor's domain and a
// resource name that is not a slice of a card.
func IsGPUResource(resource string) bool {
	domain, name, ok := strings.Cut(resource, "/")
	if !ok {
		return false
	}
	switch domain {
	case "nvidia.com":
		return name == "gpu"
	case "huawei.com":
		return strings.HasPrefix(name, "Ascend")
	case "amd.com":
		return name == "gpu"
	}
	return false
}
