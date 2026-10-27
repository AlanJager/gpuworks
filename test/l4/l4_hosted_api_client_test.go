//go:build cloudgpu && (compshare || aliyun)

package l4

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
	"gpuworks.ai/gpuworks/internal/testenv/sshutil"
)

type hostedForward interface {
	BaseURL() string
	Close() error
}

type hostedForwardFactory func(context.Context) (hostedForward, error)

type hostedSSHTunnel struct{ tunnel *sshutil.ServerDialTunnel }

func (f *hostedSSHTunnel) BaseURL() string { return "http://" + f.tunnel.Addr() }
func (f *hostedSSHTunnel) Close() error    { return f.tunnel.Close() }

func hostedSSHForwardFactory(instance *cloudprovider.Instance, remoteAddr string) hostedForwardFactory {
	return func(ctx context.Context) (hostedForward, error) {
		var tunnel *sshutil.ServerDialTunnel
		var err error
		if instance.Password != "" {
			tunnel, err = sshutil.NewServerDialTunnelPassword(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.Password, remoteAddr)
		} else {
			tunnel, err = sshutil.NewServerDialTunnel(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.PrivateKey, remoteAddr)
		}
		if err != nil {
			return nil, err
		}
		return &hostedSSHTunnel{tunnel: tunnel}, nil
	}
}

type hostedAPIClient struct {
	mu       sync.Mutex
	factory  hostedForwardFactory
	forward  hostedForward
	token    string
	username string
	password string
	client   *http.Client
}

func newHostedAPIClient(factory hostedForwardFactory, username, password string) *hostedAPIClient {
	return &hostedAPIClient{factory: factory, username: username, password: password, client: &http.Client{Timeout: 30 * time.Second}}
}

func (c *hostedAPIClient) GetJSON(ctx context.Context, path string, output any) error {
	return c.doJSON(ctx, http.MethodGet, path, nil, output, true)
}

func (c *hostedAPIClient) PostJSON(ctx context.Context, path string, input, output any) error {
	return c.doJSON(ctx, http.MethodPost, path, input, output, false)
}

func (c *hostedAPIClient) PutJSON(ctx context.Context, path string, input, output any) error {
	return c.doJSON(ctx, http.MethodPut, path, input, output, false)
}

func (c *hostedAPIClient) PatchJSON(ctx context.Context, path string, input, output any) error {
	return c.doJSON(ctx, http.MethodPatch, path, input, output, false)
}

// hostedAPIError is a non-2xx answer of the management API. Its message is the
// one the client always reported; Code is the stable product error code
// ({"code": "IMAGE_ARCHIVE_URL_UNREACHABLE", ...}) a case branches on.
type hostedAPIError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *hostedAPIError) Error() string {
	return fmt.Sprintf("hosted API %s %s returned HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// Code is the product error code in the response body, or "".
func (e *hostedAPIError) Code() string {
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(e.Body), &body)
	return body.Code
}

// hostedAPIErrorCode reports the product error code err carries, or "".
func hostedAPIErrorCode(err error) string {
	var apiErr *hostedAPIError
	if errors.As(err, &apiErr) {
		return apiErr.Code()
	}
	return ""
}

func (c *hostedAPIClient) doJSON(ctx context.Context, method, requestPath string, input, output any, retryForward bool) error {
	if !strings.HasPrefix(requestPath, "/") || strings.HasPrefix(requestPath, "//") {
		return errors.New("hosted API path must be absolute-path relative")
	}
	tries := 1
	if retryForward {
		tries = 2
	}
	var lastErr error
	for attempt := 0; attempt < tries; attempt++ {
		baseURL, token, err := c.session(ctx)
		if err != nil {
			lastErr = err
			c.DropForward()
			continue
		}
		var body io.Reader
		if input != nil {
			encoded, err := json.Marshal(input)
			if err != nil {
				return err
			}
			body = bytes.NewReader(encoded)
		}
		request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+requestPath, body)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		if input != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := c.client.Do(request)
		if err != nil {
			lastErr = err
			c.DropForward()
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<20))
		response.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if retryForward {
				c.DropForward()
				continue
			}
			return readErr
		}
		if response.StatusCode == http.StatusUnauthorized && retryForward {
			lastErr = errors.New("hosted API authentication expired")
			c.DropForward()
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return &hostedAPIError{Method: method, Path: requestPath, Status: response.StatusCode, Body: strings.TrimSpace(string(data))}
		}
		if output == nil || len(bytes.TrimSpace(data)) == 0 {
			return nil
		}
		// A forwarding connection can disappear after the HTTP headers have
		// arrived yet before the JSON body is complete. Depending on how the
		// remote server framed the response, io.ReadAll does not always surface
		// that as a transport error. Treat syntactically incomplete JSON on an
		// idempotent request as a dead forward and retry it once. Valid JSON with
		// a DTO/type mismatch is an API contract error and must not be hidden by
		// a reconnect.
		if !json.Valid(data) && retryForward {
			lastErr = errors.New("hosted API returned incomplete JSON")
			c.DropForward()
			continue
		}
		return unmarshalHostedJSON(data, output)
	}
	return fmt.Errorf("hosted API forward unavailable: %w", lastErr)
}

// unmarshalHostedJSON decodes into a fresh value before replacing output.
// Polling callers deliberately reuse DTO variables, while the server omits
// zero-valued fields such as node_rank=0 and headless=false. Decoding directly
// into the reused value would retain non-zero fields from the prior response
// and can make a leader appear to carry a follower's rank metadata.
func unmarshalHostedJSON(data []byte, output any) error {
	target := reflect.ValueOf(output)
	if target.Kind() != reflect.Pointer || target.IsNil() {
		return json.Unmarshal(data, output)
	}
	fresh := reflect.New(target.Elem().Type())
	if err := json.Unmarshal(data, fresh.Interface()); err != nil {
		return err
	}
	target.Elem().Set(fresh.Elem())
	return nil
}

func (c *hostedAPIClient) session(ctx context.Context) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.forward == nil {
		forward, err := c.factory(ctx)
		if err != nil {
			return "", "", err
		}
		c.forward = forward
		c.token = ""
	}
	if c.token == "" {
		payload, _ := json.Marshal(map[string]string{"username": c.username, "password": c.password})
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.forward.BaseURL()+"/api/v1/auth/login", bytes.NewReader(payload))
		if err != nil {
			return "", "", err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := c.client.Do(request)
		if err != nil {
			return "", "", err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return "", "", fmt.Errorf("hosted API login returned HTTP %d", response.StatusCode)
		}
		var login struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&login); err != nil || login.Token == "" {
			return "", "", errors.New("hosted API login returned no token")
		}
		c.token = login.Token
	}
	return c.forward.BaseURL(), c.token, nil
}

func (c *hostedAPIClient) DropForward() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.forward != nil {
		_ = c.forward.Close()
	}
	c.forward = nil
	c.token = ""
}

func (c *hostedAPIClient) Close() { c.DropForward() }
