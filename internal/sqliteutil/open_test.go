package sqliteutil

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlite "github.com/glebarez/go-sqlite"
)

func cantOpen(t *testing.T, dsn string) error {
	t.Helper()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	err = db.Ping()
	var coded *sqlite.Error
	if !errors.As(err, &coded) || coded.Code()&255 != 14 {
		t.Fatalf("want actual SQLITE_CANTOPEN, got %v", err)
	}
	return err
}

func TestWrapOpenErrorRealFilesystemFailures(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprint(directory), func(t *testing.T) {
			path := t.TempDir()
			want := "is a directory"
			if !directory {
				path = filepath.Join(path, "missing", "gpuworks.db")
				want = "parent directory does not exist"
			}
			cause := cantOpen(t, path)
			err := WrapOpenError(path, fmt.Errorf("initialize database: %w", cause))
			for _, text := range []string{path, "SQLITE_CANTOPEN", "initialize database", want} {
				if !strings.Contains(err.Error(), text) {
					t.Errorf("%q missing %q", err, text)
				}
			}
			if strings.Contains(err.Error(), "out of memory") {
				t.Errorf("misleading memory message: %v", err)
			}
			var original, got *sqlite.Error
			errors.As(cause, &original)
			if !errors.As(err, &got) || got != original || !errors.Is(err, cause) {
				t.Fatal("driver error chain lost")
			}
			if !directory {
				if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("diagnostic created parent: %v", err)
				}
			}
			if again := WrapOpenError("unrelated.db", err); again != err {
				t.Fatal("diagnostic is not idempotent")
			}
		})
	}
}

type codeError struct {
	code int
	text string
}

func (e *codeError) Error() string { return e.text }
func (e *codeError) Code() int     { return e.code }

func TestWrapOpenErrorLeavesOtherErrorsAlone(t *testing.T) {
	for _, err := range []error{nil, errors.New("unable to open database file: out of memory (14)"), os.ErrPermission,
		&codeError{7, "out of memory (7)"}, &codeError{5, "database is locked (5)"}, &codeError{8, "readonly (8)"}, &codeError{26, "not a database (26)"}, &codeError{13, "database is full (13)"}} {
		if got := WrapOpenError("test.db", err); got != err {
			t.Errorf("changed non-CANTOPEN %v to %v", err, got)
		}
	}
}

func TestWrapOpenErrorDSNs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "out of memory %23 db")
	u := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&token=do-not-display"}).String()
	for _, tc := range []struct {
		name, dsn, want string
		memory          bool
	}{
		{"plain", path + "?token=do-not-display", path, false},
		{"file URI", u, path, false},
		{"relative file URI", "file:relative%20db?mode=rwc&token=do-not-display", "relative db", false},
		{"empty", "", "in-memory", true},
		{"memory", ":memory:", "in-memory", true},
		{"memory URI", "file::memory:?cache=shared", "in-memory", true},
		{"named memory", "file:example?mode=memory&cache=shared", "in-memory", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause := &codeError{14, "unable to open database file: out of memory (14)"}
			err := WrapOpenError(tc.dsn, fmt.Errorf("connect: %w", cause))
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "SQLITE_CANTOPEN") {
				t.Fatalf("missing path/code: %v", err)
			}
			if strings.Contains(err.Error(), "do-not-display") {
				t.Fatalf("query leaked: %v", err)
			}
			if tc.memory && strings.Contains(err.Error(), "parent directory") {
				t.Fatalf("file advice for memory database: %v", err)
			}
			if !errors.Is(err, cause) {
				t.Fatal("lost cause")
			}
		})
	}
}

func TestWrapOpenErrorExtendedCodeAndExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.db")
	if err := os.WriteFile(path, []byte("preserve"), 0400); err != nil {
		t.Fatal(err)
	}
	cause := &codeError{14 | 0x100, "extended cannot open"}
	err := WrapOpenError("file:"+path+"?mode=ro", cause)
	if !strings.Contains(err.Error(), "SQLITE_CANTOPEN") || !strings.Contains(err.Error(), "read permission") {
		t.Fatalf("missing read-only advice: %v", err)
	}
	if strings.Contains(err.Error(), "write permission") {
		t.Fatalf("wrong access advice for read-only DSN: %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != "preserve" {
		t.Fatalf("diagnostic touched file: %q %v", got, readErr)
	}
}
