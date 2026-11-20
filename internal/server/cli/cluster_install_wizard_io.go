package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// selfinstall prompts accept EOF as an empty answer. A wizard must instead
// stop when the operator closes input, especially while retrying a field.
type clusterWizardEOFReader struct{ io.Reader }

func (r clusterWizardEOFReader) Read(p []byte) (int, error) {
	if r.Reader == nil {
		return 0, io.ErrUnexpectedEOF
	}
	// Keep the shared prompt reader from reading ahead into a password.
	if len(p) > 1 {
		p = p[:1]
	}
	n, err := r.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func clusterWizardPrompt(ctx context.Context, prompts *selfinstall.IO, question string, password bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Disable echo synchronously before launching a reader. Cancellation can
	// restore the terminal without racing a later transition in that goroutine.
	if password {
		if f, ok := prompts.In.(*os.File); ok {
			state, err := term.MakeRaw(int(f.Fd()))
			if err != nil {
				return "", err
			}
			defer func() { _ = term.Restore(int(f.Fd()), state) }()
		}
	}
	type answer struct {
		value string
		err   error
	}
	replies := make(chan answer, 1)
	go func() {
		var value string
		var err error
		if password {
			fmt.Fprint(prompts.Out, question)
			value, err = readClusterWizardPassword(prompts.In)
			fmt.Fprint(prompts.Out, "\r\n")
		} else {
			value, err = prompts.Prompt(question)
		}
		replies <- answer{value, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case reply := <-replies:
		return reply.value, reply.err
	}
}

// Raw terminal mode makes Ctrl-D an explicit byte and Ctrl-C a cancellation.
// Read one byte at a time so a second password line stays in the input stream.
// Preserve spaces exactly, matching --password-file semantics.
func readClusterWizardPassword(input io.Reader) (string, error) {
	reader := bufio.NewReader(clusterWizardEOFReader{input})
	var password []byte
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return "", err
		}
		switch b {
		case '\r', '\n':
			return string(password), nil
		case 3:
			return "", context.Canceled
		case 4:
			return "", io.ErrUnexpectedEOF
		case 8, 127:
			if len(password) > 0 {
				_, size := utf8.DecodeLastRune(password)
				password = password[:len(password)-size]
			}
		case 21:
			password = password[:0]
		default:
			password = append(password, b)
			if len(password) > 4096 {
				return "", errors.New("password input exceeds 4096 bytes")
			}
		}
	}
}

func clusterWizardValue(ctx context.Context, prompts *selfinstall.IO, question, fallback string, validate func(string) error) (string, error) {
	for {
		label := question + ": "
		if fallback != "" {
			label = fmt.Sprintf("%s [%s]: ", question, fallback)
		}
		value, err := clusterWizardPrompt(ctx, prompts, label, false)
		if err != nil {
			return "", err
		}
		if value == "" {
			value = fallback
		}
		if err := validate(value); err != nil {
			prompts.Warn("输入无效：%v", err)
			continue
		}
		return value, nil
	}
}

func clusterWizardConfirm(ctx context.Context, prompts *selfinstall.IO, question string, want bool) (bool, error) {
	fallback := "n"
	if want {
		fallback = "y"
	}
	value, err := clusterWizardValue(ctx, prompts, question+"（y/n）", fallback, func(s string) error {
		switch strings.ToLower(s) {
		case "y", "yes", "n", "no":
			return nil
		}
		return errors.New("请输入 y 或 n")
	})
	return strings.EqualFold(value, "y") || strings.EqualFold(value, "yes"), err
}

func saveClusterWizardConfig(ctx context.Context, path string, raw []byte, previous os.FileInfo, original []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cluster-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	// Use the same strict parser as installation before publishing any file.
	options := clusterInstallOptions{HTTP: 8800, Enrollment: 9443, Database: 9001}
	if err := readClusterInstallConfig(f.Name(), &options, nil); err != nil {
		return err
	}
	if err := validateClusterInstallHosts(&options); err != nil {
		return err
	}
	if err := validateClusterInstallPorts(options.HTTP, options.Enrollment, options.Database); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if previous == nil {
		// Same-directory linking publishes atomically and never replaces a file
		// created by another process after the wizard's initial inspection.
		return os.Link(f.Name(), path)
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(previous, current) {
		return errors.New("configuration changed while editing; existing file was retained")
	}
	now, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(now, original) {
		return errors.New("configuration contents changed while editing; existing file was retained")
	}
	return os.Rename(f.Name(), path)
}

func clusterWizardPassword(ctx context.Context, prompts, streams *selfinstall.IO, specified, parent string) (string, error) {
	if specified == "" {
		specified = filepath.Join(parent, "cluster-admin.password")
	}
	path, err := clusterWizardValue(ctx, prompts, "管理员密码文件（不存在时引导创建，密码不写入 YAML）", specified, func(s string) error {
		if s == "" {
			return errors.New("请输入密码文件路径")
		}
		_, err := clusterWizardLocalPath(s)
		return err
	})
	if err != nil {
		return "", err
	}
	path, err = clusterWizardLocalPath(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(path); err == nil {
		var options clusterInstallOptions
		if err := readClusterInstallPassword(path, &options); err != nil {
			return "", err
		}
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	secret := prompts
	if _, ok := streams.In.(*os.File); ok {
		secret = streams
	}
	password, err := promptClusterInstallPassword(ctx, secret)
	if err != nil {
		return "", err
	}
	if err := clusterInstallPrivateFile(path, []byte(password)); err != nil {
		return "", err
	}
	prompts.Say("已创建私有密码文件 %s（0600）。", path)
	return path, nil
}

func promptClusterInstallPassword(ctx context.Context, streams *selfinstall.IO) (string, error) {
	for {
		password, err := clusterWizardPrompt(ctx, streams, "初始管理员密码（隐藏输入，至少 8 字节）: ", true)
		if err != nil {
			return "", err
		}
		if len(password) < 8 || len(password) > 4096 || strings.ContainsAny(password, "\r\n\x00") {
			streams.Warn("密码须为单行、8 到 4096 字节。")
			continue
		}
		confirmation, err := clusterWizardPrompt(ctx, streams, "再次输入管理员密码: ", true)
		if err != nil {
			return "", err
		}
		if confirmation != password {
			streams.Warn("两次密码不一致，请重试。")
			continue
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return password, nil
	}
}
