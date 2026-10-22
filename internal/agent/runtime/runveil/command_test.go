package runveil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// container builds a udocker-layout container directory: ROOT plus the image
// config blob as container.json.
func container(t *testing.T, config map[string]any) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "containers", "inst-1")
	root := filepath.Join(dir, "ROOT")
	for _, d := range []string{"bin", "etc", "root", "workspace", "usr/local/bin", "usr/bin", "home/app"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("bin/sh", "#!", 0o755)
	write("usr/local/bin/serve", "#!", 0o755)
	write("usr/local/bin/data.txt", "not executable", 0o644)
	write("etc/passwd", "root:x:0:0:root:/root:/bin/bash\napp:x:1000:1000::/home/app:/bin/sh\n", 0o644)
	write("etc/group", "root:x:0:\napp:x:1000:\n", 0o644)
	// An absolute symlink must resolve inside the guest, never on the host.
	if err := os.Symlink("/usr/local/bin/serve", filepath.Join(root, "usr/bin/python3")); err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(map[string]any{"architecture": "amd64", "config": config})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "container.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func baseSpec(dir string) Spec {
	return Spec{
		Proot:        "/opt/proot",
		ContainerDir: dir,
		Name:         "inst-1-long-name",
		Kernel:       "6.8.0-90-generic",
		HostEnv:      []string{"TERM=xterm", "PATH=/host/bin", "LD_LIBRARY_PATH=/host/lib", "PROOT_TMP_DIR=/var/tmp", "AWS_SECRET=x"},
		Identity:     Identity{UID: 1002, GID: 1002, Groups: []int{44, 109}, Username: "udocker"},
	}
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		out[k] = v
	}
	return out
}

func TestPrepareRootWorkloadMatchesUdockerP1(t *testing.T) {
	dir := container(t, map[string]any{
		"Entrypoint": []string{"python3", "-m", "vllm"},
		"Cmd":        []string{"--help"},
		"Env":        []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HF_HOME=/image", "LANG=C.UTF-8"},
		"WorkingDir": "/workspace",
	})
	host := t.TempDir()
	spec := baseSpec(dir)
	spec.Env = map[string]string{"HF_HOME": "/caller", "CUDA_VISIBLE_DEVICES": "3"}
	spec.Binds = []Bind{{Host: host, Guest: "/models/"}}
	spec.Cmd = []string{"serve", "/models/x"}

	command, err := Prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(filepath.Join(dir, "ROOT"))
	want := []string{"/opt/proot", "--kill-on-exit", "-b", host + ":/models"}
	for _, sys := range SystemBinds {
		if _, err := os.Stat(sys); err == nil {
			want = append(want, "-b", sys+":"+sys)
		}
	}
	want = append(want,
		"-b", filepath.Join(dir, passwdFile)+":/etc/passwd",
		"-b", filepath.Join(dir, groupFile)+":/etc/group",
		"-0", "-k", "6.8.0-90-generic", "-r", root, "-w", "/workspace",
		// Workload Cmd replaces the image Cmd but keeps the image entrypoint.
		"python3", "-m", "vllm", "serve", "/models/x")
	if !slices.Equal(command.Argv, want) {
		t.Fatalf("argv\n got %q\nwant %q", command.Argv, want)
	}
	env := envMap(command.Env)
	for k, v := range map[string]string{
		"HF_HOME":              "/caller", // caller wins over the image
		"LANG":                 "C.UTF-8", // image fills unset keys
		"CUDA_VISIBLE_DEVICES": "3",
		"PATH":                 "/usr/local/bin:/usr/bin:/bin", // image PATH beats host PATH
		"TERM":                 "xterm",
		"PROOT_TMP_DIR":        "/var/tmp",
		"HOME":                 "/root",
		"USER":                 "root",
		"LOGNAME":              "root",
		"PS1":                  "inst-1-l# ",
		"container_execmode":   "P1",
		"container_ruser":      "udocker",
		"container_root":       root,
	} {
		if env[k] != v {
			t.Errorf("env %s=%q, want %q", k, env[k], v)
		}
	}
	for _, leaked := range []string{"LD_LIBRARY_PATH", "AWS_SECRET"} {
		if _, ok := env[leaked]; ok {
			t.Errorf("host variable %s leaked into the container", leaked)
		}
	}
	if !slices.IsSorted(command.Env) {
		t.Errorf("env is not in a stable order: %q", command.Env)
	}
}

func TestPrepareEntrypointOverrideDropsImageCmd(t *testing.T) {
	dir := container(t, map[string]any{"Entrypoint": []string{"python3"}, "Cmd": []string{"image-default"}})
	spec := baseSpec(dir)
	spec.Entrypoint = []string{"/bin/sh", "-c"}
	spec.Cmd = []string{"echo ok"}
	command, err := Prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	tail := command.Argv[len(command.Argv)-3:]
	if !slices.Equal(tail, []string{"/bin/sh", "-c", "echo ok"}) {
		t.Fatalf("argv tail %q", tail)
	}
	// No WorkingDir in the image: udocker falls back to the account home.
	if i := slices.Index(command.Argv, "-w"); command.Argv[i+1] != "/root" {
		t.Fatalf("workdir %q, want /root", command.Argv[i+1])
	}

	spec.Entrypoint, spec.Cmd = nil, nil
	command, err = Prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	if tail := command.Argv[len(command.Argv)-2:]; !slices.Equal(tail, []string{"python3", "image-default"}) {
		t.Fatalf("image defaults not used: %q", tail)
	}
}

func TestPrepareNonRootImageUserMapsToHostIdentity(t *testing.T) {
	dir := container(t, map[string]any{"User": "app", "Cmd": []string{"serve"}, "Env": []string{}})
	command, err := Prepare(baseSpec(dir))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(command.Argv, "-0") {
		t.Fatal("non-root image user must not get fake root")
	}
	if i := slices.Index(command.Argv, "-i"); i < 0 || command.Argv[i+1] != "1002:1002" {
		t.Fatalf("uid map missing or wrong: %q", command.Argv)
	}
	env := envMap(command.Env)
	if env["HOME"] != "/home/app" || env["USER"] != "app" || env["PATH"] != userPath || env["PS1"] != `inst-1-l\$ ` {
		t.Fatalf("user env %v", env)
	}
	passwd, _ := os.ReadFile(filepath.Join(dir, passwdFile))
	if !strings.Contains(string(passwd), "app:x:1002:1002:*UDOCKER*:/home/app:/bin/sh\n") {
		t.Fatalf("mapped account missing from passwd:\n%s", passwd)
	}
	group, _ := os.ReadFile(filepath.Join(dir, groupFile))
	for _, line := range []string{"app:x:1002:\n", "G44:x:44:app,\n", "G109:x:109:app,\n"} {
		if !strings.Contains(string(group), line) {
			t.Errorf("group file lacks %q:\n%s", line, group)
		}
	}
	// Relaunching must not grow the generated files.
	if _, err := Prepare(baseSpec(dir)); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(dir, passwdFile))
	if string(again) != string(passwd) {
		t.Fatalf("passwd changed on relaunch:\n%s\n---\n%s", passwd, again)
	}
}

func TestPrepareAccountEdgeCases(t *testing.T) {
	id := Identity{UID: 1002, GID: 1003}
	for _, tc := range []struct {
		user          string
		wantUID       string
		wantGID       string
		wantName      string
		wantHome      string
		wantErrSubstr string
	}{
		{user: "", wantUID: "0", wantGID: "0", wantName: "root", wantHome: "/root"},
		{user: "app:app", wantUID: "1002", wantGID: "1003", wantName: "app", wantHome: "/home/app"},
		{user: "1000", wantUID: "1002", wantGID: "1003", wantName: "app", wantHome: "/home/app"},
		{user: "4242:77", wantUID: "4242", wantGID: "77", wantName: "udoc4242", wantHome: "/"},
		{user: "ghost", wantUID: "1002", wantGID: "1003", wantName: "ghost", wantHome: "/"},
		{user: "bad user!", wantErrSubstr: "invalid image user"},
	} {
		dir := container(t, nil)
		account, _, err := PrepareAccount(dir, filepath.Join(dir, "ROOT"), tc.user, id)
		if tc.wantErrSubstr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("user %q: err %v, want %q", tc.user, err, tc.wantErrSubstr)
			}
			continue
		}
		if err != nil {
			t.Errorf("user %q: %v", tc.user, err)
			continue
		}
		if account.UID != tc.wantUID || account.GID != tc.wantGID || account.User != tc.wantName || account.Home != tc.wantHome {
			t.Errorf("user %q: got %+v", tc.user, account)
		}
	}
}

func TestPrepareMissingPasswdStillProducesAccount(t *testing.T) {
	dir := container(t, map[string]any{"Cmd": []string{"/bin/sh"}})
	if err := os.Remove(filepath.Join(dir, "ROOT", "etc", "passwd")); err != nil {
		t.Fatal(err)
	}
	command, err := Prepare(baseSpec(dir))
	if err != nil {
		t.Fatal(err)
	}
	// udocker: root not found in the guest means uid falls back to the host.
	if i := slices.Index(command.Argv, "-i"); i < 0 || command.Argv[i+1] != "1002:1002" {
		t.Fatalf("argv %q", command.Argv)
	}
}

func TestPrepareRejectsInvalidLaunches(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]any
		mutate func(*Spec)
		want   string
	}{
		{"missing host volume", map[string]any{"Cmd": []string{"/bin/sh"}}, func(s *Spec) {
			s.Binds = []Bind{{Host: "/definitely/not/here", Guest: "/data"}}
		}, "invalid host volume path"},
		{"relative host volume", map[string]any{"Cmd": []string{"/bin/sh"}}, func(s *Spec) {
			s.Binds = []Bind{{Host: "rel", Guest: "/data"}}
		}, "invalid host volume path"},
		{"root guest volume", map[string]any{"Cmd": []string{"/bin/sh"}}, func(s *Spec) {
			s.Binds = []Bind{{Host: "/", Guest: "/"}}
		}, "invalid container volume path"},
		{"no command", map[string]any{}, nil, "has no command"},
		{"command not in PATH", map[string]any{"Cmd": []string{"nope"}}, nil, "command not found"},
		{"not executable", map[string]any{"Cmd": []string{"/usr/local/bin/data.txt"}}, nil, "command not found"},
		{"bad workdir", map[string]any{"Cmd": []string{"/bin/sh"}, "WorkingDir": "/missing"}, nil, "invalid working directory"},
		{"no proot", map[string]any{"Cmd": []string{"/bin/sh"}}, func(s *Spec) { s.Proot = "" }, "proot executable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := baseSpec(container(t, tc.config))
			if tc.mutate != nil {
				tc.mutate(&spec)
			}
			_, err := Prepare(spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := Prepare(Spec{Proot: "/p", ContainerDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "no root filesystem") {
		t.Fatalf("missing ROOT: %v", err)
	}
}

func TestHostPathResolvesGuestSymlinksAndBinds(t *testing.T) {
	dir := container(t, nil)
	root := filepath.Join(dir, "ROOT")
	models := t.TempDir()
	if err := os.WriteFile(filepath.Join(models, "run"), []byte("#!"), 0o755); err != nil {
		t.Fatal(err)
	}
	binds := []Bind{{Host: models, Guest: "/models"}}
	if got := hostPath(root, binds, "/usr/bin/python3"); got != filepath.Join(root, "usr/local/bin/serve") {
		t.Fatalf("absolute guest link resolved to %q", got)
	}
	if got := hostPath(root, binds, "/models/run"); got != filepath.Join(models, "run") {
		t.Fatalf("bind resolved to %q", got)
	}
	if !findExecutable(root, binds, "run", "/nothing:/models", "/") {
		t.Fatal("PATH search through a bind failed")
	}
	if !findExecutable(root, binds, "./serve", "", "/usr/local/bin") {
		t.Fatal("./ relative to cwd failed")
	}
	// A loop must terminate.
	if err := os.Symlink("/loop", filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	if got := hostPath(root, nil, "/loop/x"); got != "" {
		t.Fatalf("symlink loop resolved to %q", got)
	}
}

func TestLoadImageConfigUdockerLookupRules(t *testing.T) {
	dir := t.TempDir()
	blob := `{"container_config":{"cmd":"a  b","env":null,"Env":["X=1"]},"User":"top"}`
	if err := os.WriteFile(filepath.Join(dir, "container.json"), []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}
	config, err := LoadImageConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(config.Cmd, []string{"a", "b"}) || !slices.Equal(config.Env, []string{"X=1"}) || config.User != "top" {
		t.Fatalf("config %+v", config)
	}
	if err := os.WriteFile(filepath.Join(dir, "container.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadImageConfig(dir); err == nil {
		t.Fatal("corrupt metadata accepted")
	}
	if _, err := LoadImageConfig(t.TempDir()); err == nil {
		t.Fatal("missing metadata accepted")
	}
}
