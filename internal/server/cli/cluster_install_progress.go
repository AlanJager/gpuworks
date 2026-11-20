package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// clusterInstallProgress owns the local transcript and the installation clock.
// Only human-readable status belongs here; SSH protocol bodies contain secrets.
type clusterInstallProgress struct {
	mu     sync.Mutex
	output io.Writer
	log    interface {
		io.WriteCloser
		Sync() error
	}
	path, password, pending   string
	started, stageAt, taskAt  time.Time
	stage, task               string
	completed, total          int
	lineStart                 bool
	outputErr, logErr         error
	finished                  bool
	finishErr                 error
	stop, heartbeatDone, done chan struct{}
}

func newClusterInstallProgress(output io.Writer, noService bool, password string) (*clusterInstallProgress, error) {
	return newClusterInstallProgressIn(output, noService, password, "/tmp", 5*time.Second)
}

func newClusterInstallProgressIn(output io.Writer, noService bool, password, baseDir string, interval time.Duration) (*clusterInstallProgress, error) {
	dir, err := os.MkdirTemp(baseDir, "gpuworks-cluster-install-")
	if err != nil {
		return nil, fmt.Errorf("create private installation log directory: %w", err)
	}
	path := filepath.Join(dir, "install.log")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		_ = os.Remove(dir)
		return nil, fmt.Errorf("create private installation log: %w", err)
	}
	if output == nil {
		output = io.Discard
	}
	p := &clusterInstallProgress{
		output: output, log: file, path: path, password: password,
		started: time.Now(), total: 8, lineStart: true,
		stop: make(chan struct{}), heartbeatDone: make(chan struct{}), done: make(chan struct{}),
	}
	if noService {
		p.total--
	}
	p.messageLocked("本地安装日志：%s（目录 0700，文件 0600；安装结束后保留）", path)
	p.messageLocked("安装进度：已完成 0/%d (0%%) | 总用时 00:00", p.total)
	if err := errors.Join(p.outputErr, p.logErr); err != nil {
		closeErr := file.Close()
		return nil, fmt.Errorf("start installation log %s: %w", path, errors.Join(err, closeErr))
	}
	go p.heartbeat(interval)
	return p, nil
}

func (p *clusterInstallProgress) Path() string {
	if p == nil {
		return ""
	}
	return p.path
}

func (p *clusterInstallProgress) StartStage(title string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished || p.completed >= p.total {
		return
	}
	p.stage, p.stageAt = title, time.Now()
	p.task, p.taskAt = "", time.Time{}
	p.statusLocked()
}

func (p *clusterInstallProgress) Task(detail string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished || p.stage == "" || p.task == detail {
		return
	}
	p.task, p.taskAt = detail, time.Now()
	p.statusLocked()
}

func (p *clusterInstallProgress) CompleteStage() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished || p.stage == "" || p.completed >= p.total {
		return
	}
	p.completed++
	now := time.Now()
	p.messageLocked("[✓] 步骤 %d/%d %s | 已完成 %d/%d (%d%%) | 本步 %s | 总用时 %s",
		p.completed, p.total, p.stage, p.completed, p.total, p.completed*100/p.total,
		clusterInstallElapsed(now.Sub(p.stageAt)), clusterInstallElapsed(now.Sub(p.started)))
	p.stage, p.task = "", ""
}

func (p *clusterInstallProgress) heartbeat(interval time.Duration) {
	defer close(p.heartbeatDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.mu.Lock()
			if !p.finished && p.stage != "" {
				p.statusLocked()
			}
			p.mu.Unlock()
		}
	}
}

func (p *clusterInstallProgress) statusLocked() {
	now := time.Now()
	detail := ""
	if p.task != "" {
		detail = fmt.Sprintf(" | 当前：%s（%s）", p.task, clusterInstallElapsed(now.Sub(p.taskAt)))
	}
	p.messageLocked("[ ] 步骤 %d/%d %s | 已完成 %d/%d (%d%%) | 本步 %s | 总用时 %s%s",
		p.completed+1, p.total, p.stage, p.completed, p.total, p.completed*100/p.total,
		clusterInstallElapsed(now.Sub(p.stageAt)), clusterInstallElapsed(now.Sub(p.started)), detail)
}

// Write keeps only a possible password prefix between calls. In particular, a
// secret split across fmt/io writes cannot escape redaction during a heartbeat.
func (p *clusterInstallProgress) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return 0, os.ErrClosed
	}
	for remaining := data; len(remaining) > 0; {
		n := min(len(remaining), 4096)
		text := p.pending + string(remaining[:n])
		remaining = remaining[n:]
		p.pending = ""
		if p.password != "" {
			// Keep the longest proper prefix of the password at the end of
			// the unconsumed text, after replacing all complete occurrences.
			var safe strings.Builder
			for {
				at := strings.Index(text, p.password)
				if at < 0 {
					break
				}
				safe.WriteString(text[:at])
				safe.WriteString("[REDACTED]")
				text = text[at+len(p.password):]
			}
			for n := min(len(text), len(p.password)-1); n > 0; n-- {
				if strings.HasSuffix(text, p.password[:n]) {
					p.pending, text = text[len(text)-n:], text[:len(text)-n]
					break
				}
			}
			safe.WriteString(text)
			text = safe.String()
		}
		p.writeTextLocked(text)
	}
	return len(data), errors.Join(p.outputErr, p.logErr)
}

func (p *clusterInstallProgress) messageLocked(format string, args ...any) {
	if !p.lineStart {
		p.writeTextLocked("\n")
	}
	text := fmt.Sprintf(format, args...)
	if p.password != "" {
		text = strings.ReplaceAll(text, p.password, "[REDACTED]")
	}
	p.writeTextLocked(text + "\n")
}

func (p *clusterInstallProgress) writeTextLocked(text string) {
	for text != "" {
		end := strings.IndexByte(text, '\n')
		if end < 0 {
			end = len(text) - 1
		}
		part := text[:end+1]
		if p.lineStart && p.logErr == nil {
			p.logErr = clusterInstallWriteString(p.log, time.Now().Format(time.RFC3339Nano)+" ")
		}
		if p.outputErr == nil {
			p.outputErr = clusterInstallWriteString(p.output, part)
		}
		if p.logErr == nil {
			p.logErr = clusterInstallWriteString(p.log, part)
		}
		p.lineStart = strings.HasSuffix(part, "\n")
		text = text[end+1:]
	}
}

func clusterInstallWriteString(w io.Writer, text string) error {
	n, err := io.WriteString(w, text)
	if err == nil && n != len(text) {
		return io.ErrShortWrite
	}
	return err
}

func (p *clusterInstallProgress) Finish(resultErr error) error {
	if p == nil {
		return resultErr
	}
	p.mu.Lock()
	if p.finished {
		p.mu.Unlock()
		<-p.done
		return p.finishErr
	}
	p.finished = true
	close(p.stop)
	p.mu.Unlock()
	<-p.heartbeatDone
	p.mu.Lock()
	defer p.mu.Unlock()
	defer close(p.done)
	if resultErr == nil && p.completed != p.total {
		resultErr = fmt.Errorf("installation stopped with incomplete steps: %d/%d", p.completed, p.total)
	}
	if p.pending != "" {
		// A final partial write may contain only the beginning of a secret.
		p.writeTextLocked("[REDACTED]")
	}
	p.pending = ""
	outcome := "安装完成"
	if resultErr == nil && (p.outputErr != nil || p.logErr != nil) {
		outcome = "安装步骤完成，但进度或日志输出失败"
	}
	if resultErr != nil {
		outcome = "安装失败"
		if errors.Is(resultErr, context.Canceled) {
			outcome = "安装已取消"
		} else if errors.Is(resultErr, context.DeadlineExceeded) {
			outcome = "安装超时"
		}
		if p.stage != "" {
			p.statusLocked()
		}
		p.messageLocked("%s：%v", outcome, resultErr)
	}
	p.messageLocked("%s：已完成 %d/%d (%d%%) | 总用时 %s", outcome, p.completed, p.total,
		p.completed*100/p.total, clusterInstallElapsed(time.Since(p.started)))
	p.messageLocked("本地安装日志：%s（已保留）", p.path)
	p.logErr = errors.Join(p.logErr, p.log.Sync(), p.log.Close())
	if p.logErr != nil {
		// The transcript itself may be unwritable; report the limitation to
		// the terminal as well as returning it to the caller.
		warning := "安装日志写入失败，日志可能不完整。\n"
		p.outputErr = errors.Join(p.outputErr, clusterInstallWriteString(p.output, warning))
	}
	p.finishErr = errors.Join(resultErr, p.outputErr, p.logErr)
	if p.finishErr != nil && p.password != "" {
		p.finishErr = clusterInstallRedactedError{cause: p.finishErr, password: p.password}
	}
	return p.finishErr
}

// Preserve error classification without leaking credentials when the command's
// caller logs the final error outside the progress writer.
type clusterInstallRedactedError struct {
	cause    error
	password string
}

func (e clusterInstallRedactedError) Error() string {
	return strings.ReplaceAll(e.cause.Error(), e.password, "[REDACTED]")
}

func (e clusterInstallRedactedError) Unwrap() error { return e.cause }

func clusterInstallElapsed(elapsed time.Duration) string {
	seconds := max(0, int(elapsed/time.Second))
	if seconds >= 3600 {
		return fmt.Sprintf("%02d:%02d:%02d", seconds/3600, seconds/60%60, seconds%60)
	}
	return fmt.Sprintf("%02d:%02d", seconds/60, seconds%60)
}
