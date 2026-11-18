package slurmboot

import (
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func sample() Params {
	return Params{ServerURL: "https://gw.example:8800", Token: "tok-abc.123_X", RunAs: "root",
		InstallDir: "/var/lib/gpuworks", ScriptDir: "/opt/gpuworks-slurm"}
}

func TestDefaultsAreValid(t *testing.T) {
	p := Params{ServerURL: "http://10.0.0.1:8800", Token: "t"}.WithDefaults()
	if p.RunAs != DefaultRunAs || p.InstallDir != DefaultInstallDir || p.ScriptDir != DefaultScriptDir {
		t.Fatalf("defaults = %+v", p)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Params{ServerURL: "http://x", Token: "t"}).Validate(); err == nil {
		t.Fatal("Validate must not fill defaults")
	}
}

func TestValidate(t *testing.T) {
	set := func(f func(p *Params)) Params {
		p := sample()
		f(&p)
		return p
	}
	ok := []struct {
		name string
		p    Params
	}{
		{"sample", sample()},
		{"http", set(func(p *Params) { p.ServerURL = "http://gw:80" })},
		{"no port", set(func(p *Params) { p.ServerURL = "https://gw.example.com" })},
		{"with path", set(func(p *Params) { p.ServerURL = "https://gw.example.com/prefix/gpuworks" })},
		{"ipv4", set(func(p *Params) { p.ServerURL = "http://10.1.2.3:8800" })},
		{"ipv6", set(func(p *Params) { p.ServerURL = "http://[fd00::1]:8800" })},
		{"login name", set(func(p *Params) { p.RunAs = "gpu-worker_1" })},
		{"underscore login", set(func(p *Params) { p.RunAs = "_svc" })},
		{"deep dir", set(func(p *Params) { p.InstallDir = "/data/a-b/c_d/e.f" })},
		{"long token", set(func(p *Params) { p.Token = strings.Repeat("a", 512) })},
	}
	for _, c := range ok {
		if err := c.p.Validate(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	bad := []struct {
		name  string
		p     Params
		field string
	}{
		{"empty url", set(func(p *Params) { p.ServerURL = "" }), "ServerURL"},
		{"no scheme", set(func(p *Params) { p.ServerURL = "gw.example:8800" }), "ServerURL"},
		{"ftp", set(func(p *Params) { p.ServerURL = "ftp://gw" }), "ServerURL"},
		{"trailing slash", set(func(p *Params) { p.ServerURL = "https://gw/" }), "ServerURL"},
		{"query", set(func(p *Params) { p.ServerURL = "https://gw/?a=b" }), "ServerURL"},
		{"fragment", set(func(p *Params) { p.ServerURL = "https://gw/x#y" }), "ServerURL"},
		{"userinfo", set(func(p *Params) { p.ServerURL = "https://u:p@gw" }), "ServerURL"},
		{"space", set(func(p *Params) { p.ServerURL = "https://gw .example" }), "ServerURL"},
		{"quote", set(func(p *Params) { p.ServerURL = "https://gw'; rm -rf /; '" }), "ServerURL"},
		{"newline", set(func(p *Params) { p.ServerURL = "https://gw\nrm -rf /" }), "ServerURL"},
		{"command substitution", set(func(p *Params) { p.ServerURL = "https://gw/$(id)" }), "ServerURL"},
		{"backtick", set(func(p *Params) { p.ServerURL = "https://gw/`id`" }), "ServerURL"},
		{"port too big", set(func(p *Params) { p.ServerURL = "https://gw:99999" }), "ServerURL"},
		{"empty token", set(func(p *Params) { p.Token = "" }), "Token"},
		{"token with quote", set(func(p *Params) { p.Token = "a'b" }), "Token"},
		{"token injection", set(func(p *Params) { p.Token = "'; rm -rf /" }), "Token"},
		{"token newline", set(func(p *Params) { p.Token = "a\nb" }), "Token"},
		{"token slash", set(func(p *Params) { p.Token = "a/b" }), "Token"},
		{"token too long", set(func(p *Params) { p.Token = strings.Repeat("a", 513) }), "Token"},
		{"empty run as", set(func(p *Params) { p.RunAs = "" }), "RunAs"},
		{"upper case", set(func(p *Params) { p.RunAs = "Root" }), "RunAs"},
		{"leading digit", set(func(p *Params) { p.RunAs = "1abc" }), "RunAs"},
		{"run as injection", set(func(p *Params) { p.RunAs = "root; id" }), "RunAs"},
		{"run as too long", set(func(p *Params) { p.RunAs = strings.Repeat("a", 33) }), "RunAs"},
		{"relative install dir", set(func(p *Params) { p.InstallDir = "var/lib" }), "InstallDir"},
		{"dotdot", set(func(p *Params) { p.InstallDir = "/var/../etc" }), "InstallDir"},
		{"dotdot suffix", set(func(p *Params) { p.InstallDir = "/var/lib/.." }), "InstallDir"},
		{"install dir quote", set(func(p *Params) { p.InstallDir = "/var/'x" }), "InstallDir"},
		{"install dir injection", set(func(p *Params) { p.InstallDir = "/x'; rm -rf /; '" }), "InstallDir"},
		{"install dir space", set(func(p *Params) { p.InstallDir = "/var/my dir" }), "InstallDir"},
		{"root dir", set(func(p *Params) { p.InstallDir = "/" }), "InstallDir"},
		{"trailing slash dir", set(func(p *Params) { p.InstallDir = "/var/lib/" }), "InstallDir"},
		{"doubled slash", set(func(p *Params) { p.ScriptDir = "/opt//x" }), "ScriptDir"},
		{"dot segment", set(func(p *Params) { p.ScriptDir = "/opt/./x" }), "ScriptDir"},
		{"script dir newline", set(func(p *Params) { p.ScriptDir = "/opt/x\n/y" }), "ScriptDir"},
		{"empty script dir", set(func(p *Params) { p.ScriptDir = "" }), "ScriptDir"},
	}
	for _, c := range bad {
		err := c.p.Validate()
		var fe *FieldError
		if !errors.As(err, &fe) {
			t.Errorf("%s: accepted (err=%v)", c.name, err)
			continue
		}
		if fe.Field != c.field || fe.Reason == "" || !errors.Is(err, ErrInvalidParams) {
			t.Errorf("%s: %+v, want field %s", c.name, fe, c.field)
		}
	}
}

func TestCommands(t *testing.T) {
	const server = "https://gw.example:8800"
	cases := []struct{ got, want string }{
		{SetupCommand(server, "abc.DEF_1-2"), "curl -fsSL 'https://gw.example:8800/slurm/setup.sh?token=abc.DEF_1-2' | sudo sh"},
		{CheckCommand(server, "abc"), "curl -fsSL 'https://gw.example:8800/slurm/setup.sh?token=abc&check=1' | sudo sh -s -- --check"},
		{RemoveCommand(server, "abc"), "curl -fsSL 'https://gw.example:8800/slurm/setup.sh?token=abc' | sudo sh -s -- --remove"},
		{UninstallAgentsCommand(server, "abc"), "curl -fsSL 'https://gw.example:8800/slurm/setup.sh?token=abc' | sudo sh -s -- --uninstall-agents"},
		// the token is URL-escaped, whatever it holds
		{SetupCommand(server, "a b&c=d/e+f"), "curl -fsSL 'https://gw.example:8800/slurm/setup.sh?token=a+b%26c%3Dd%2Fe%2Bf' | sudo sh"},
		// a single quote in the URL cannot end the quoting
		{SetupCommand("https://h/'x", "t"), `curl -fsSL 'https://h/'\''x/slurm/setup.sh?token=t' | sudo sh`},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got  %s\nwant %s", c.got, c.want)
		}
	}
}

func TestScriptsAreRenderedWithQuotedParameters(t *testing.T) {
	p := sample()
	for name, s := range map[string]string{"check": CheckScript(p), "setup": SetupScript(p)} {
		for _, want := range []string{
			"SERVER_URL='https://gw.example:8800'\n", "RUN_AS='root'\n",
			"INSTALL_DIR='/var/lib/gpuworks'\n", "SCRIPT_DIR='/opt/gpuworks-slurm'\n",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
		if strings.Contains(s, paramsMarker) {
			t.Errorf("%s still has the parameter placeholder", name)
		}
	}
	// only the setup script holds the token; the check script is world readable
	if !strings.Contains(SetupScript(p), "TOKEN='tok-abc.123_X'\n") {
		t.Error("the setup script lacks the token")
	}
	if strings.Contains(CheckScript(p), "tok-abc.123_X") || strings.Contains(CheckScript(p), "TOKEN='") {
		t.Error("the check script carries the token")
	}
	if n := strings.Count(SetupScript(p), "tok-abc.123_X"); n != 1 {
		t.Errorf("the token appears %d times in the setup script, want once (its parameter)", n)
	}
	// even unvalidated input cannot break out of the quoting
	evil := Params{ServerURL: "https://x'; echo pwned; '", Token: "t'; echo pwned; '", RunAs: "root", InstallDir: "/i", ScriptDir: "/s"}
	if !strings.Contains(SetupScript(evil), `TOKEN='t'\''; echo pwned; '\'''`+"\n") {
		t.Error("a single quote in the token is not escaped")
	}
	if !strings.Contains(CheckScript(evil), `SERVER_URL='https://x'\''; echo pwned; '\'''`+"\n") {
		t.Error("a single quote in a value is not escaped in the check script")
	}
}

func TestScriptsAreStrictPOSIXShell(t *testing.T) {
	p := sample()
	for name, s := range map[string]string{"check": CheckScript(p), "setup": SetupScript(p)} {
		if !strings.HasPrefix(s, "#!/bin/sh\n") {
			t.Errorf("%s does not start with #!/bin/sh", name)
		}
		if !strings.Contains(s, "\nset -u\n") {
			t.Errorf("%s lacks set -u", name)
		}
		// [[:space:]] is a POSIX character class, not a bash test
		bare := regexp.MustCompile(`\[:[a-z]+:\]`).ReplaceAllString(s, "")
		for _, bad := range []string{"[[", "]]", "echo -e", "echo -n", "<<<", "declare ", "=~", "${!", "pushd", "$RANDOM", "&>"} {
			if strings.Contains(bare, bad) {
				t.Errorf("%s contains the non-POSIX %q", name, bad)
			}
		}
		for _, bad := range []*regexp.Regexp{regexp.MustCompile(`(?m)^\s*local\s`), regexp.MustCompile(`(?m)^\s*source\s`)} {
			if bad.MatchString(s) {
				t.Errorf("%s contains the non-POSIX %v", name, bad)
			}
		}
		// no arrays: name=( ... )
		if regexp.MustCompile(`\w=\(`).MatchString(s) {
			t.Errorf("%s uses an array", name)
		}
	}
}

func TestSetupEmbedsTheCheckScriptWithADelimiterThatCannotOccurInIt(t *testing.T) {
	const delim = "GWKS_CHECK_SCRIPT_EOF"
	p := sample()
	check := CheckScript(p)
	if strings.Contains(check, delim) || strings.Contains(check, checkMarker) {
		t.Fatal("the check script contains the here-document delimiter")
	}
	setup := SetupScript(p)
	if strings.Count(setup, "<<'"+delim+"'\n") != 1 || strings.Count(setup, "\n"+delim+"\n") != 1 {
		t.Fatalf("setup must have exactly one quoted here-document")
	}
	body := setup[strings.Index(setup, "<<'"+delim+"'\n")+len("<<'"+delim+"'\n"):]
	body = body[:strings.Index(body, "\n"+delim+"\n")+1]
	if body != check {
		t.Fatal("the embedded check script differs from CheckScript")
	}
}

func TestScriptsParseUnderEveryShell(t *testing.T) {
	dir := t.TempDir()
	p := sample()
	for _, sh := range shells(t) {
		for name, s := range map[string]string{"check": CheckScript(p), "setup": SetupScript(p), "check-template": checkTemplate, "setup-template": setupSource()} {
			file := filepath.Join(dir, name+".sh")
			w := &world{t: t}
			w.write(file, s)
			if out, err := exec.Command(sh.path, "-n", file).CombinedOutput(); err != nil {
				t.Errorf("%s -n %s: %v\n%s", sh.name, name, err, out)
			}
		}
	}
	// shellcheck, when the machine has it, is stricter than any shell
	if sc, err := exec.LookPath("shellcheck"); err == nil {
		for _, name := range []string{"check", "setup"} {
			if out, err := exec.Command(sc, "-s", "sh", filepath.Join(dir, name+".sh")).CombinedOutput(); err != nil {
				t.Errorf("shellcheck %s:\n%s", name, out)
			}
		}
	}
}

func TestFilesStayUnderTheLengthLimit(t *testing.T) {
	for name, s := range map[string]string{"check.sh": checkTemplate, "setup.sh": setupTemplate, "setup_conf.sh": setupConfPart, "setup_nodes.sh": setupNodesPart} {
		if n := strings.Count(s, "\n"); n > 600 {
			t.Errorf("%s has %d lines", name, n)
		}
	}
}
