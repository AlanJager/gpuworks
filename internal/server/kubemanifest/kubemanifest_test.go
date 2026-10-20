package kubemanifest

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "rewrite testdata/manifest.golden")

func sample() Params {
	return Params{
		ClusterName: "prod", Namespace: "gpuworks-system", ServerURL: "https://gw.example:8800",
		Token: "enroll-token", AgentImage: "registry.example/gpuworks/gpuworks-agent:v1.2.3",
		NodeSelector: map[string]string{"nvidia.com/gpu.present": "true", "gpuworks.ai/pool": "a"},
		Vendor:       "nvidia", RuntimeClass: "nvidia", EnginePorts: "40000-42767",
	}
}

func docs(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(s))
	for {
		var d map[string]any
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("manifest is not valid YAML: %v\n%s", err, s)
		}
		out = append(out, d)
	}
}

func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("at %v: not a map: %#v", path, v)
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				t.Fatalf("at %v: not a list long enough: %#v", path, v)
			}
			v = l[k]
		}
	}
	return v
}

func TestRenderGolden(t *testing.T) {
	got, err := Render(sample())
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "manifest.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(got), want) {
		t.Fatalf("manifest differs from %s (run with -update after checking):\n%s", golden, got)
	}
}

func TestRenderedObjects(t *testing.T) {
	out, err := Render(sample())
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]map[string]any{}
	for _, d := range docs(t, out) {
		kinds[d["kind"].(string)] = d
	}
	for _, k := range []string{"Namespace", "ServiceAccount", "Secret", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding", "PriorityClass", "DaemonSet"} {
		if kinds[k] == nil {
			t.Fatalf("manifest lacks a %s", k)
		}
	}
	if got := dig(t, kinds["Namespace"], "metadata", "labels", "pod-security.kubernetes.io/enforce"); got != "privileged" {
		t.Fatalf("namespace PSA label = %v", got)
	}
	ds := kinds["DaemonSet"]
	spec := dig(t, ds, "spec", "template", "spec")
	if dig(t, spec, "hostNetwork") != true || dig(t, spec, "dnsPolicy") != "Default" || dig(t, spec, "runtimeClassName") != "nvidia" ||
		dig(t, spec, "nodeSelector", "gpuworks.ai/pool") != "a" || dig(t, spec, "serviceAccountName") != AgentName {
		t.Fatalf("daemonset pod spec = %#v", spec)
	}
	args := dig(t, spec, "containers", 0, "args").([]any)
	want := []string{"run", "--server=https://gw.example:8800", "--state=/opt/gpuworks", "--driver=kube", "--engine-ports=40000-42767"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %v, want %v", args, want)
		}
	}
	mounts := dig(t, spec, "containers", 0, "volumeMounts").([]any)
	if dig(t, mounts[0], "mountPath") != DefaultStateDir || dig(t, mounts[0], "mountPropagation") != "Bidirectional" {
		t.Fatalf("state mount = %v", mounts[0])
	}
	if dig(t, kinds["Secret"], "stringData", "token") != "enroll-token" {
		t.Fatal("secret does not carry the token")
	}
}

func TestRenderWithoutOptionalFields(t *testing.T) {
	p := sample()
	p.NodeSelector, p.RuntimeClass, p.EnginePorts = nil, "", ""
	out, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs(t, out) {
		if d["kind"] != "DaemonSet" {
			continue
		}
		spec := dig(t, d, "spec", "template", "spec").(map[string]any)
		if _, ok := spec["runtimeClassName"]; ok {
			t.Fatal("runtimeClassName rendered without one")
		}
		if _, ok := spec["nodeSelector"]; ok {
			t.Fatal("nodeSelector rendered without one")
		}
	}
}

// TestRenderAscend: an Ascend cluster's agents run without the NVIDIA
// RuntimeClass (even if one is named) and mount the host's /usr/local, where
// npu-smi and the driver live, plus the install record the probe reads.
func TestRenderAscend(t *testing.T) {
	p := sample()
	p.Vendor, p.StateDir, p.ContainerdSocket = "ascend", "/opt/gpuworks-kube", "/run/k3s/containerd/containerd.sock"
	p.APIServer = "https://10.0.0.5:6443"
	out, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	var ds map[string]any
	for _, d := range docs(t, out) {
		if d["kind"] == "DaemonSet" {
			ds = d
		}
	}
	spec := dig(t, ds, "spec", "template", "spec").(map[string]any)
	if _, ok := spec["runtimeClassName"]; ok {
		t.Fatal("an Ascend agent must not run under the NVIDIA RuntimeClass")
	}
	c := dig(t, spec, "containers", 0).(map[string]any)
	if dig(t, c, "args", 2) != "--state=/opt/gpuworks-kube" {
		t.Fatalf("args = %v", c["args"])
	}
	env := map[string]any{}
	for _, e := range c["env"].([]any) {
		env[dig(t, e, "name").(string)] = e.(map[string]any)["value"]
	}
	if _, ok := env["NVIDIA_VISIBLE_DEVICES"]; ok {
		t.Fatal("NVIDIA env on an Ascend agent")
	}
	if env["GPUWORKS_KUBE_HOST_CLIS"] != "1" || env["GPUWORKS_KUBE_CONTAINERD_SOCKET"] != "/run/k3s/containerd/containerd.sock" ||
		env["KUBERNETES_SERVICE_HOST"] != "10.0.0.5" || env["KUBERNETES_SERVICE_PORT"] != "6443" {
		t.Fatalf("env = %v", env)
	}
	mounts := map[string]map[string]any{}
	for _, m := range c["volumeMounts"].([]any) {
		mounts[dig(t, m, "mountPath").(string)] = m.(map[string]any)
	}
	for _, path := range []string{"/opt/gpuworks-kube", "/usr/local", "/etc/ascend_install.info", DevicePluginDir, PodResourcesDir} {
		if mounts[path] == nil {
			t.Fatalf("no mount at %s: %v", path, mounts)
		}
	}
	if mounts["/usr/local"]["readOnly"] != true || mounts[DevicePluginDir]["readOnly"] == true {
		t.Fatalf("mount modes: /usr/local %v, device plugins %v", mounts["/usr/local"], mounts[DevicePluginDir])
	}
	types := map[string]any{}
	for _, v := range spec["volumes"].([]any) {
		types[dig(t, v, "hostPath", "path").(string)] = dig(t, v, "hostPath", "type")
	}
	if types["/usr/local"] != "Directory" || types["/etc/ascend_install.info"] != "File" || types["/opt/gpuworks-kube"] != "DirectoryOrCreate" {
		t.Fatalf("volume types = %v", types)
	}
}

func TestValuesCannotChangeTheStructure(t *testing.T) {
	p := sample()
	p.ClusterName = "x\n---\nkind: Pod"
	p.Token = "a\"\nb: c"
	out, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	ds := docs(t, out)
	if len(ds) != 9 {
		t.Fatalf("documents = %d, want 9", len(ds))
	}
	for _, d := range ds {
		if d["kind"] == "Secret" && dig(t, d, "stringData", "token") != p.Token {
			t.Fatalf("token round trip = %q", dig(t, d, "stringData", "token"))
		}
	}
}

func TestValidate(t *testing.T) {
	for name, mut := range map[string]func(*Params){
		"namespace":    func(p *Params) { p.Namespace = "Bad_NS" },
		"server":       func(p *Params) { p.ServerURL = "gw.example" },
		"token":        func(p *Params) { p.Token = " " },
		"image":        func(p *Params) { p.AgentImage = "" },
		"runtimeclass": func(p *Params) { p.RuntimeClass = "No" },
		"socket":       func(p *Params) { p.ContainerdSocket = "run/containerd.sock" },
		"state":        func(p *Params) { p.StateDir = "/" },
		"state-rel":    func(p *Params) { p.StateDir = "opt/x" },
		"state-dirty":  func(p *Params) { p.StateDir = "/opt/../x" },
		"vendor":       func(p *Params) { p.Vendor = "apple" },
		"apiserver":    func(p *Params) { p.APIServer = "http://10.0.0.5:6443" },
		"apiserver-np": func(p *Params) { p.APIServer = "10.0.0.5" },
		"no-vendor":    func(p *Params) { p.Vendor = "" },
	} {
		p := sample()
		mut(&p)
		if _, err := Render(p); err == nil {
			t.Fatalf("%s: invalid parameters rendered", name)
		}
	}
}

func TestParseNodeSelector(t *testing.T) {
	got, err := ParseNodeSelector(" nvidia.com/gpu.present=true , pool=a ,")
	if err != nil || len(got) != 2 || got["nvidia.com/gpu.present"] != "true" || got["pool"] != "a" {
		t.Fatalf("ParseNodeSelector = %v, %v", got, err)
	}
	if got, err := ParseNodeSelector(""); err != nil || len(got) != 0 {
		t.Fatalf("empty = %v, %v", got, err)
	}
	for _, bad := range []string{"novalue", "a=b c", "=x"} {
		if _, err := ParseNodeSelector(bad); err == nil {
			t.Fatalf("ParseNodeSelector(%q) accepted", bad)
		}
	}
}

func TestParseAPIServer(t *testing.T) {
	for in, want := range map[string][2]string{
		"https://10.0.0.5:6443":     {"10.0.0.5", "6443"},
		"https://api.example/":      {"api.example", "443"},
		"10.0.0.5:6443":             {"10.0.0.5", "6443"},
		"[fd00::1]:6443":            {"fd00::1", "6443"},
		"https://[fd00::1]:6443":    {"fd00::1", "6443"},
		" https://k8s.local:16443 ": {"k8s.local", "16443"},
	} {
		host, port, err := ParseAPIServer(in)
		if err != nil || host != want[0] || port != want[1] {
			t.Errorf("ParseAPIServer(%q) = %q %q %v", in, host, port, err)
		}
	}
	for _, bad := range []string{"http://10.0.0.5:6443", "10.0.0.5", "host:0", "host:99999", ":6443", "https://a b:1"} {
		if _, _, err := ParseAPIServer(bad); err == nil {
			t.Errorf("ParseAPIServer(%q) accepted", bad)
		}
	}
}

// TestRenderWithoutAPIServer: the Service the kubelet injects is used.
func TestRenderWithoutAPIServer(t *testing.T) {
	out, err := Render(sample())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "KUBERNETES_SERVICE_HOST") {
		t.Fatal("API server override rendered without one")
	}
}
