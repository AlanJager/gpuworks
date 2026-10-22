package provision

import (
	"strings"
	"testing"
)

func TestParseTargetAcceptsPastedSSHCommands(t *testing.T) {
	tests := []struct {
		name   string
		target string
		wantU  string
		wantH  string
		wantP  int
	}{
		{name: "port flag and trailing forward", target: "ssh -p 51223 root@203.0.113.7 -L 8080:localhost:8080", wantU: "root", wantH: "203.0.113.7", wantP: 51223},
		{name: "plain destination", target: "ssh user@de1d8928258b45b9.spark-21.ssh.enverge.dev", wantU: "user", wantH: "de1d8928258b45b9.spark-21.ssh.enverge.dev", wantP: 22},
		{name: "glued port and login flag", target: "ssh -p2222 -l ubuntu gpu.example", wantU: "ubuntu", wantH: "gpu.example", wantP: 2222},
		{name: "switch cluster before arg option", target: "ssh -tvp 2200 root@gpu.example", wantU: "root", wantH: "gpu.example", wantP: 2200},
		{name: "identity and options ignored", target: `ssh -i ~/.ssh/runpod -o StrictHostKeyChecking=no -o "Port 40022" root@gpu.example`, wantU: "root", wantH: "gpu.example", wantP: 40022},
		{name: "option user", target: "ssh -o User=admin gpu.example", wantU: "admin", wantH: "gpu.example", wantP: 22},
		{name: "remote command ignored", target: "ssh root@gpu.example -p 2022 nvidia-smi -L", wantU: "root", wantH: "gpu.example", wantP: 2022},
		{name: "shell separator ends command", target: "ssh -p 51223 root@203.0.113.7; ssh user@other.example", wantU: "root", wantH: "203.0.113.7", wantP: 51223},
		{name: "ssh uri", target: "ssh ssh://root@gpu.example:2222", wantU: "root", wantH: "gpu.example", wantP: 2222},
		{name: "bare ssh uri", target: "ssh://root@[2001:db8::1]:2022", wantU: "root", wantH: "2001:db8::1", wantP: 2022},
		{name: "absolute ssh path", target: "/usr/bin/ssh -p 23 root@gpu.example", wantU: "root", wantH: "gpu.example", wantP: 23},
		{name: "ipv6 destination", target: "ssh -p 2022 root@2001:db8::1", wantU: "root", wantH: "2001:db8::1", wantP: 2022},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, h, p, err := ParseTarget(tt.target, "", 0)
			if err != nil {
				t.Fatal(err)
			}
			if u != tt.wantU || h != tt.wantH || p != tt.wantP {
				t.Fatalf("ParseTarget = %q/%q/%d, want %q/%q/%d", u, h, p, tt.wantU, tt.wantH, tt.wantP)
			}
		})
	}
}

func TestParseTargetRejectsUnusableSSHCommands(t *testing.T) {
	tests := []struct {
		target string
		want   string
	}{
		{target: "ssh", want: "no destination"},
		{target: "ssh -p", want: "-p needs a value"},
		{target: "ssh -p abc root@gpu.example", want: `invalid port "abc"`},
		{target: "ssh -p 70000 root@gpu.example", want: "invalid port"},
		{target: "ssh -J bastion root@gpu.example", want: "jump hosts"},
		{target: "ssh -o ProxyJump=bastion root@gpu.example", want: "proxyjump is not supported"},
		{target: "ssh -o HostName=10.0.0.1 alias", want: "HostName is not supported"},
		{target: "ssh 'root@gpu.example", want: "unterminated quote"},
		{target: "ssh gpu.example", want: "ssh user is required"},
		{target: "root@gpu.example:abc", want: `invalid port "abc"`},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			_, _, _, err := ParseTarget(tt.target, "", 0)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}
