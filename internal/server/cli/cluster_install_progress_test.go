package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClusterInstallProgressRetainsPrivateTranscript(t *testing.T) {
	failure := errors.New("node could not start")
	for _, tc := range []struct {
		name, outcome string
		noService     bool
		result        error
	}{
		{"success", "安装完成", false, nil},
		{"no service", "安装完成", true, nil},
		{"failure", "安装失败", false, failure},
		{"cancel", "安装已取消", false, context.Canceled},
		{"timeout", "安装超时", false, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			p, err := newClusterInstallProgressIn(&out, tc.noService, "", t.TempDir(), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Finish(context.Canceled) })
			for path, mode := range map[string]os.FileMode{p.Path(): 0600, filepath.Dir(p.Path()): 0700} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != mode {
					t.Fatalf("private path %s: info=%v, error=%v, expected mode=%o", path, info, err, mode)
				}
			}
			total := 8
			if tc.noService {
				total = 7
			}
			if !strings.Contains(out.String(), p.Path()) || !strings.Contains(out.String(), fmt.Sprintf("已完成 0/%d (0%%)", total)) {
				t.Fatalf("initial progress omitted path or denominator: %s", &out)
			}
			stages := total
			if tc.result != nil {
				stages = 2
			}
			for n := 0; n < stages; n++ {
				p.StartStage(fmt.Sprintf("阶段 %d", n+1))
				p.Task("节点 1/3 node-1：启动服务")
				if strings.Contains(out.String(), "(100%)") {
					t.Fatal("reported 100% before final stage completed")
				}
				p.CompleteStage()
			}
			if tc.result != nil {
				p.StartStage("故障阶段")
				p.Task("节点 2/3 node-2：等待服务")
			}
			if err := p.Finish(tc.result); !errors.Is(err, tc.result) {
				t.Fatalf("result lost error identity: %v", err)
			}
			want := fmt.Sprintf("%s：已完成 %d/%d (%d%%)", tc.outcome, stages, total, stages*100/total)
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing final progress %q: %s", want, &out)
			}
			if tc.result != nil && !strings.Contains(out.String(), "故障阶段") {
				t.Fatal("failed stage missing")
			}
			raw, err := os.ReadFile(p.Path())
			if err != nil {
				t.Fatalf("transcript not retained: %v", err)
			}
			var plain strings.Builder
			for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
				stamp, text, ok := strings.Cut(line, " ")
				if _, err := time.Parse(time.RFC3339Nano, stamp); !ok || err != nil {
					t.Fatalf("line has no valid timestamp: %q", line)
				}
				plain.WriteString(text + "\n")
			}
			if plain.String() != out.String() {
				t.Fatal("timestamped transcript differs from terminal output")
			}
		})
	}
}

func TestClusterInstallProgressRedactsSplitSecrets(t *testing.T) {
	const secret = "private-password-测试"
	var out bytes.Buffer
	p, err := newClusterInstallProgressIn(&out, false, secret, t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Finish(context.Canceled) })
	p.StartStage("运行检查")
	for split := 0; split <= len(secret); split++ {
		for _, chunk := range []string{"first line\ncredential=", secret[:split], secret[split:], "\n"} {
			if n, err := io.WriteString(p, chunk); err != nil || n != len(chunk) {
				t.Fatalf("write: n=%d error=%v", n, err)
			}
			p.Task(fmt.Sprintf("检查块 %d/%d", split, len(chunk)))
		}
	}
	// Exercise the fixed-size input chunks and a final unterminated line.
	_, _ = io.WriteString(p, strings.Repeat("x", 4093)+secret+strings.Repeat("y", 8200)+"\npartial final line")
	p.Task("controlled error containing " + secret)
	_ = p.Finish(fmt.Errorf("failure containing %s", secret))
	raw, err := os.ReadFile(p.Path())
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"terminal": out.String(), "log": string(raw)} {
		if strings.Contains(text, secret) {
			t.Fatalf("%s leaked password", name)
		}
		if got := strings.Count(text, "[REDACTED]"); got != len(secret)+5 {
			t.Fatalf("%s replacement count=%d, want=%d", name, got, len(secret)+5)
		}
		if !strings.Contains(text, "partial final line\n") {
			t.Fatalf("%s lost final partial line", name)
		}
	}
}

func TestClusterInstallProgressHeartbeatStopsAndTasksKeepElapsed(t *testing.T) {
	out := &clusterProgressTestOutput{changed: make(chan struct{}, 1)}
	p, err := newClusterInstallProgressIn(out, false, "private-password", t.TempDir(), 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Finish(context.Canceled) })
	p.StartStage("加入其余节点")
	p.Task("节点 2/3 node-2：等待成员加入")
	p.mu.Lock()
	started := p.taskAt
	p.mu.Unlock()
	before := strings.Count(out.String(), "当前：")
	p.Task("节点 2/3 node-2：等待成员加入")
	p.mu.Lock()
	if p.taskAt != started {
		t.Fatal("repeated task reset elapsed time")
	}
	p.mu.Unlock()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for strings.Count(out.String(), "当前：") <= before {
		select {
		case <-out.changed:
		case <-deadline.C:
			t.Fatal("no heartbeat while task was waiting")
		}
	}
	var writers sync.WaitGroup
	for n := 0; n < 20; n++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			_, _ = io.WriteString(p, "check result private-password\n")
		}()
	}
	writers.Wait()
	var finishers sync.WaitGroup
	for n := 0; n < 4; n++ {
		finishers.Add(1)
		go func() {
			defer finishers.Done()
			if err := p.Finish(context.Canceled); !errors.Is(err, context.Canceled) {
				t.Errorf("concurrent finish lost result: %v", err)
			}
		}()
	}
	finishers.Wait()
	select {
	case <-p.heartbeatDone:
	default:
		t.Fatal("Finish returned before heartbeat exited")
	}
	final := out.String()
	p.StartStage("late stage")
	p.Task("late task")
	p.CompleteStage()
	if n, err := p.Write([]byte("late output")); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write after close: n=%d, error=%v", n, err)
	}
	if out.String() != final || strings.Contains(final, "private-password") {
		t.Fatal("output changed after Finish or exposed password")
	}
}

func TestClusterInstallProgressRedactsReturnedErrorsAndFinalSecretPrefix(t *testing.T) {
	const secret = "private-password-for-error"
	for _, original := range []error{
		errors.New("failure containing " + secret),
		fmt.Errorf("operation with %s: %w", secret, context.Canceled),
	} {
		var out bytes.Buffer
		p, err := newClusterInstallProgressIn(&out, false, secret, t.TempDir(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Finish(context.Canceled) })
		prefix := secret[:len(secret)-1]
		if _, err := io.WriteString(p, "credential="+prefix); err != nil {
			t.Fatal(err)
		}
		err = p.Finish(original)
		if !errors.Is(err, original) || errors.Is(err, context.Canceled) != errors.Is(original, context.Canceled) {
			t.Fatalf("redaction lost error identity or cancellation classification: %v", err)
		}
		raw, readErr := os.ReadFile(p.Path())
		if readErr != nil {
			t.Fatal(readErr)
		}
		for name, text := range map[string]string{"returned error": err.Error(), "terminal": out.String(), "log": string(raw)} {
			if strings.Contains(text, secret) || strings.Contains(text, prefix) || !strings.Contains(text, "[REDACTED]") {
				t.Fatalf("%s did not redact the credential", name)
			}
		}
	}
}

type clusterProgressTestOutput struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	changed chan struct{}
}

func (w *clusterProgressTestOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return n, err
}

func (w *clusterProgressTestOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestClusterInstallProgressPropagatesLogFailures(t *testing.T) {
	fault := errors.New("log storage unavailable")
	for _, operation := range []string{"write", "sync", "close", "short write"} {
		t.Run(operation, func(t *testing.T) {
			var out bytes.Buffer
			p, err := newClusterInstallProgressIn(&out, false, "", t.TempDir(), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Finish(context.Canceled) })
			p.mu.Lock()
			p.log = &clusterProgressFaultLog{File: p.log.(*os.File), operation: operation, fault: fault}
			p.mu.Unlock()
			p.StartStage("分发程序")
			want := fault
			if operation == "short write" {
				want = io.ErrShortWrite
			}
			err = p.Finish(context.Canceled)
			if !errors.Is(err, want) || !errors.Is(err, context.Canceled) {
				t.Fatalf("lost log or installation error: %v", err)
			}
			if !strings.Contains(out.String(), "日志可能不完整") {
				t.Fatal("failed log storage was not reported")
			}
			if _, err := os.Stat(p.Path()); err != nil {
				t.Fatalf("existing log removed on failure: %v", err)
			}
		})
	}
}

type clusterProgressFaultLog struct {
	*os.File
	operation string
	fault     error
}

func (w *clusterProgressFaultLog) Write(p []byte) (int, error) {
	if w.operation == "write" {
		return 0, w.fault
	}
	if w.operation == "short write" {
		return len(p) / 2, nil
	}
	return w.File.Write(p)
}

func (w *clusterProgressFaultLog) WriteString(text string) (int, error) {
	return w.Write([]byte(text))
}

func (w *clusterProgressFaultLog) Sync() error {
	if w.operation == "sync" {
		return w.fault
	}
	return w.File.Sync()
}

func (w *clusterProgressFaultLog) Close() error {
	err := w.File.Close()
	if w.operation == "close" {
		return errors.Join(err, w.fault)
	}
	return err
}

func TestClusterInstallProgressRefusesMissingLogAndIncompleteSuccess(t *testing.T) {
	base := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(base, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if p, err := newClusterInstallProgressIn(io.Discard, false, "", base, time.Hour); p != nil || err == nil {
		t.Fatal("installation allowed without private transcript")
	}
	var out bytes.Buffer
	p, err := newClusterInstallProgressIn(&out, false, "", t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p.StartStage("预检查")
	p.CompleteStage()
	p.CompleteStage()
	if err := p.Finish(nil); err == nil {
		t.Fatal("reported successful installation with incomplete steps")
	}
	if strings.Contains(out.String(), "安装完成") || !strings.Contains(out.String(), "已完成 1/8 (12%)") {
		t.Fatalf("incomplete result was misleading: %s", &out)
	}
	var absent *clusterInstallProgress
	absent.StartStage("预检查")
	absent.Task("node-1")
	absent.CompleteStage()
	if !errors.Is(absent.Finish(context.Canceled), context.Canceled) || absent.Path() != "" {
		t.Fatal("nil reporter changed result")
	}
}

func TestClusterInstallProgressReportsTerminalFailuresInTranscript(t *testing.T) {
	base := t.TempDir()
	if p, err := newClusterInstallProgressIn(clusterProgressShortWriter{}, false, "", base, time.Hour); p != nil || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("constructor accepted failed terminal: reporter=%v, error=%v", p, err)
	}
	logs, err := filepath.Glob(filepath.Join(base, "gpuworks-cluster-install-*", "install.log"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("constructor failure lost created log: %v, %v", logs, err)
	}
	p, err := newClusterInstallProgressIn(io.Discard, true, "", t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Finish(context.Canceled) })
	p.mu.Lock()
	p.output = clusterProgressShortWriter{}
	p.mu.Unlock()
	for n := 0; n < 7; n++ {
		p.StartStage("已执行阶段")
		p.CompleteStage()
	}
	if err := p.Finish(nil); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("lost output failure: %v", err)
	}
	raw, err := os.ReadFile(p.Path())
	if err != nil || !strings.Contains(string(raw), "安装步骤完成，但进度或日志输出失败") {
		t.Fatalf("transcript misrepresented failed reporting: %s, error=%v", raw, err)
	}
}

type clusterProgressShortWriter struct{}

func (clusterProgressShortWriter) Write(data []byte) (int, error) {
	return len(data) / 2, nil
}
