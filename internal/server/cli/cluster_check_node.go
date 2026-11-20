package cli

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

func clusterCheckNodeCommand(ctx context.Context, input io.Reader, output io.Writer) error {
	return clusterCheckNodeRunDetailed(ctx, input, output, func(ctx context.Context, req clusterCheckRequest, checks map[string]string) error {
		return validateClusterCheckNodeDetailed(ctx, req, clusterInstallNative, clusterInstallProcessSupport, checks)
	})
}

func clusterCheckNodeRun(ctx context.Context, input io.Reader, output io.Writer, validate func(context.Context, clusterCheckRequest) error) error {
	return clusterCheckNodeRunDetailed(ctx, input, output, func(ctx context.Context, req clusterCheckRequest, _ map[string]string) error {
		return validate(ctx, req)
	})
}

func clusterCheckNodeRunDetailed(ctx context.Context, input io.Reader, output io.Writer, validate func(context.Context, clusterCheckRequest, map[string]string) error) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	encoder := json.NewEncoder(output)
	var req clusterCheckRequest
	if err := decoder.Decode(&req); err != nil {
		return fmt.Errorf("read check request: %w", err)
	}
	if req.Action != "prepare" {
		return errors.New("check helper requires prepare first")
	}
	var listeners []net.Listener
	var pending sync.WaitGroup
	defer func() {
		cancel()
		for _, listener := range listeners {
			listener.Close()
		}
		pending.Wait()
	}()
	checks := make(map[string]string)
	err := validate(ctx, req, checks)
	if err == nil {
		for _, port := range []int{req.HTTP, req.Enrollment, req.Database, req.Database + 1} {
			listener, bindErr := net.Listen("tcp4", net.JoinHostPort(req.Host, strconv.Itoa(port)))
			if bindErr != nil {
				bindErr = fmt.Errorf("bind %s:%d (address must be local and port free): %w", req.Host, port, bindErr)
				checks["port:"+strconv.Itoa(port)] = bindErr.Error()
				err = errors.Join(err, bindErr)
				continue
			}
			checks["port:"+strconv.Itoa(port)] = ""
			listeners = append(listeners, listener)
			pending.Add(1)
			go func(l net.Listener) { defer pending.Done(); clusterCheckServe(ctx, l, req.Token) }(listener)
		}
	}
	if err != nil {
		return encoder.Encode(clusterCheckResponse{Error: err.Error(), Checks: checks})
	}
	if err := encoder.Encode(clusterCheckResponse{Checks: checks}); err != nil {
		return err
	}
	type decoded struct {
		request clusterCheckRequest
		err     error
	}
	commands := make(chan decoded)
	// A canceled helper may stop reading stdin. Only the helper-owned listeners
	// and temporary files are released; no installation process is touched.
	go func() {
		defer close(commands)
		for {
			var command clusterCheckRequest
			err := decoder.Decode(&command)
			select {
			case commands <- decoded{command, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case item, ok := <-commands:
			if !ok || item.err == io.EOF {
				return nil
			}
			if item.err != nil {
				return fmt.Errorf("read check command: %w", item.err)
			}
			command := item.request
			if command.Action == "stop" {
				return encoder.Encode(clusterCheckResponse{})
			}
			err := errors.New("unknown check command")
			if command.Action == "probe" {
				if command.Token != req.Targets[command.Target] || req.Targets[command.Target] == "" || (command.Port != req.Enrollment && command.Port != req.Database && command.Port != req.Database+1) {
					err = errors.New("probe token or port differs from prepared check")
				} else if ip := net.ParseIP(command.Target); ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() {
					err = errors.New("probe target must be a usable IPv4 address")
				} else {
					err = clusterCheckProbe(ctx, command.Target, command.Port, command.Token)
				}
			}
			response := clusterCheckResponse{}
			if command.Action == "storage" {
				response.Storage, err = clusterCheckNodeStorage(ctx, req.Dir, command.StorageProbeMS)
			}
			if err != nil {
				response.Error = err.Error()
			}
			if err := encoder.Encode(response); err != nil {
				return err
			}
		}
	}
}

func clusterCheckServe(ctx context.Context, listener net.Listener, token string) {
	var connections sync.WaitGroup
	defer connections.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(clusterCheckProbeTimeout))
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			defer stop()
			raw, err := bufio.NewReader(io.LimitReader(conn, 66)).ReadString('\n')
			if err == nil && subtle.ConstantTimeCompare([]byte(raw), []byte(token+"\n")) == 1 {
				_, _ = io.WriteString(conn, "gpuworks-check:"+token+"\n")
			}
		}()
	}
}

func clusterCheckProbe(ctx context.Context, host string, port int, token string) error {
	probeCtx, cancel := context.WithTimeout(ctx, clusterCheckProbeTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp4", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, _ := probeCtx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(probeCtx, func() { conn.Close() })
	defer stop()
	if _, err := io.WriteString(conn, token+"\n"); err != nil {
		return err
	}
	raw, err := bufio.NewReader(io.LimitReader(conn, 96)).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read authenticated probe: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(raw), []byte("gpuworks-check:"+token+"\n")) != 1 {
		return errors.New("listener did not authenticate this check challenge")
	}
	return nil
}

// clusterCheckNodeStorage measures the filesystem under the prepared
// installation directory. A failure is this node's answer, not the helper's.
func clusterCheckNodeStorage(ctx context.Context, installDir string, milliseconds int) (*clusterCheckStorage, error) {
	duration := time.Duration(milliseconds) * time.Millisecond
	if duration <= 0 || duration > clusterCheckStorageLimit {
		return nil, fmt.Errorf("storage probe duration must be within %s", clusterCheckStorageLimit)
	}
	dir, err := clusterStorageDirectory(installDir)
	if err != nil {
		return nil, fmt.Errorf("storage probe directory: %w", err)
	}
	result, err := newClusterStorageProbe().run(ctx, dir, duration)
	if err != nil {
		return nil, err
	}
	return &result, nil
}
