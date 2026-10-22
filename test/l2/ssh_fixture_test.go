//go:build multiproc

package l2

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"strconv"
	"testing"

	"golang.org/x/crypto/ssh"
)

type sshCommandHandler func(command string) (stdout, stderr string, status uint32)
type sshSessionHandler func(command string, channel ssh.Channel) (stdout, stderr string, status uint32)

// startSSHCommandFixture provides the real SSH transport used by out-of-band
// recovery while keeping the remote host hermetic. Each exec request is routed
// through handler and returns normal stdout/stderr/exit-status SSH semantics.
func startSSHCommandFixture(t *testing.T, user, password string, handler sshCommandHandler) (string, int) {
	return startSSHSessionFixture(t, user, password, func(command string, _ ssh.Channel) (string, string, uint32) {
		return handler(command)
	})
}

func startSSHSessionFixture(t *testing.T, user, password string, handler sshSessionHandler) (string, int) {
	t.Helper()
	_, hostPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PasswordCallback: func(metadata ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if metadata.User() != user || string(pass) != password {
				return nil, fmt.Errorf("password rejected")
			}
			return nil, nil
		},
	}
	config.AddHostKey(hostSigner)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go serveSSHFixtureConn(conn, config, handler)
		}
	}()

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func serveSSHFixtureConn(conn net.Conn, config *ssh.ServerConfig, handler sshSessionHandler) {
	serverConn, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer serverConn.Close()
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "session channels only")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer channel.Close()
			for request := range channelRequests {
				if request.Type != "exec" {
					_ = request.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
					_ = request.Reply(false, nil)
					return
				}
				_ = request.Reply(true, nil)
				stdout, stderr, status := handler(payload.Command, channel)
				_, _ = channel.Write([]byte(stdout))
				_, _ = channel.Stderr().Write([]byte(stderr))
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{status}))
				return
			}
		}()
	}
}
