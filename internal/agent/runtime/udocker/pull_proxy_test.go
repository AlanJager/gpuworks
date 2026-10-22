package udocker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestCLIPullEnforcesNetworkChoiceAfterRunUserLoginProfile(t *testing.T) {
	for _, proxy := range []string{"", "http://user:p%27ass%3B%24%28x%29@selected.example:3128"} {
		name := "direct"
		if proxy != "" {
			name = "proxy"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("GPUWORKS_UDOCKER_DISABLE_NATIVE", "1")
			base := t.TempDir()
			environment := filepath.Join(base, "pull-env")
			args := filepath.Join(base, "runuser-args")
			// runuser starts a login shell whose profile may restore these values.
			// Model that externally visible behavior before executing the actual body.
			runuser := "#!/bin/sh\nset -eu\n[ \"$1\" = '-u' ] && [ \"$3\" = '--' ] && [ \"$4\" = 'bash' ] && [ \"$5\" = '-lc' ] || exit 42\nprintf '%s' \"$6\" > '" + args + "'\nexport HTTP_PROXY=http://profile.invalid:3128 http_proxy=http://profile.invalid:3128 HTTPS_PROXY=http://profile.invalid:3128 https_proxy=http://profile.invalid:3128 ALL_PROXY=socks5://profile.invalid:1080 all_proxy=socks5://profile.invalid:1080 NO_PROXY='*' no_proxy='*'\nexec /bin/sh -c \"$6\"\n"
			if err := os.WriteFile(filepath.Join(base, "runuser"), []byte(runuser), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", base+string(os.PathListSeparator)+os.Getenv("PATH"))
			d := New(base)
			d.provisioned = true
			d.runUser = "fixture"
			d.udockerExe = filepath.Join(base, "udocker")
			if err := os.WriteFile(d.udockerExe, []byte("#!/bin/sh\n[ \"$*\" = '--allow-root pull registry.example/model:v1' ] || exit 42\nenv > '"+environment+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := d.PullImage(runtime.WithPullProxy(context.Background(), proxy), "registry.example/model:v1", nil); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(environment)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			if strings.Contains(text, "profile.invalid") || strings.Contains(text, "NO_PROXY=*") || strings.Contains(text, "no_proxy=*") {
				t.Fatal("login profile overrode the selected image network path")
			}
			if proxy != "" && (!strings.Contains(text, "HTTP_PROXY="+proxy+"\n") || !strings.Contains(text, "HTTPS_PROXY="+proxy+"\n")) {
				t.Fatal("selected proxy was not restored after login profile")
			}
			command, err := os.ReadFile(args)
			if err != nil {
				t.Fatal(err)
			}
			if proxy != "" && strings.Contains(string(command), proxy) {
				t.Fatal("proxy credentials were placed in process arguments")
			}
		})
	}
}
