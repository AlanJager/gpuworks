package storagerules

import (
	"strings"
	"testing"
)

func TestValidateMountOptions(t *testing.T) {
	ok := map[string]string{
		"":                "",
		"   ":             "",
		"rw,noatime":      "rw,noatime",
		" ro , vers=4.2 ": "ro,vers=4.2",
		"rw,\tnoatime\t":  "rw,noatime", // an item's surrounding whitespace is trimmed
		"rw,rw,hard,hard": "rw,hard",
		"vers=4.1,rsize=1048576,proto=tcp,sec=sys,timeo=600,nconnect=8": "vers=4.1,rsize=1048576,proto=tcp,sec=sys,timeo=600,nconnect=8",
		"clientaddr=fe80::1":  "clientaddr=fe80::1",
		"local_lock=none,fsc": "local_lock=none,fsc",
	}
	for in, want := range ok {
		got, err := ValidateMountOptions(in)
		if err != nil || got != want {
			t.Errorf("ValidateMountOptions(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := map[string]string{ // input -> text the error must mention
		// What the old server check refused too.
		"rw;rm -rf /": "rw;rm -rf /",
		"rw noatime":  "rw noatime",
		"rsize=$HOME": "rsize=$HOME",
		"rsize=`id`":  "rsize=`id`",
		"proto=tcp;x": "proto=tcp;x",
		"rsize=1 2":   "rsize=1 2",
		"rw|cat":      "rw|cat",
		"vers=4&x":    "vers=4&x",
		"rw>out":      "rw>out",
		"rw<in":       "rw<in",
		// What the old server accepted and the agent refused at mount time.
		"rw,suid":             "suid",
		"vers=":               "vers=",
		"rw,,noatime":         "empty",
		"rw,":                 "empty",
		",rw":                 "empty",
		"ro,rw":               "ro and rw",
		"vers=3,vers=4":       "vers=4",
		"RW":                  "RW",
		"rw,sec=sys\nnoatime": "noatime",
		"=x":                  "=x",
		"nosuchoption":        "nosuchoption",
		"rsize=1048576=2":     "rsize=1048576=2",
		"sec=sys\x00":         "sec=sys",
		"vers=4.2,bogus=1":    "bogus=1",
	}
	for in, mention := range bad {
		got, err := ValidateMountOptions(in)
		if err == nil {
			t.Errorf("ValidateMountOptions(%q) = %q, want error", in, got)
			continue
		}
		if !strings.Contains(err.Error(), mention) {
			t.Errorf("ValidateMountOptions(%q) error %q should mention %q", in, err, mention)
		}
	}
}

func TestValidateExport(t *testing.T) {
	for _, export := range []string{
		"10.0.0.5:/export/models",
		"nfs.example.internal:/export",
		"nfs-1:/",
		"[fe80::1]:/srv/models",
		"host:/export/with-dash_and.dot/dir",
		"host:/export/ünïcode",
	} {
		if err := ValidateExport(export); err != nil {
			t.Errorf("ValidateExport(%q) = %v, want ok", export, err)
		}
	}
	bad := map[string]string{ // input -> text the error must mention
		"":                 "empty",
		"-o:/x":            "'-'",
		"-host:/x":         "'-'",
		"no-colon":         "host:/path",
		"host:path":        "host:/path", // the old agent accepted it, mount needs an absolute path
		":/export":         "host:/path",
		"host name:/x":     "whitespace",
		"host:/a b":        "whitespace",
		"host:/a\tb":       "whitespace",
		"host:/a\nb":       "whitespace",
		"host:/a\x01":      "control",
		"host:/a\x7f":      "control",
		"host:/a;reboot":   "host:/path", // the old agent accepted it
		"host:/$HOME":      "host:/path",
		"host:/`id`":       "host:/path",
		"ho$t:/x":          "host:/path",
		"host:/x:y name/z": "whitespace",
	}
	for export, mention := range bad {
		err := ValidateExport(export)
		if err == nil {
			t.Errorf("ValidateExport(%q) = nil, want error", export)
			continue
		}
		if !strings.Contains(err.Error(), mention) {
			t.Errorf("ValidateExport(%q) error %q should mention %q", export, err, mention)
		}
	}
}
