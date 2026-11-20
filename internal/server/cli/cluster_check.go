package cli

import (
	"context"
	"crypto/rand"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Check traffic has a random per-run challenge, so an unrelated listener cannot
// satisfy a connectivity check. It contains no installation credentials.
type clusterCheckRequest struct {
	Action     string            `json:"action"`
	Dir        string            `json:"dir,omitempty"`
	Host       string            `json:"host,omitempty"`
	User       string            `json:"user,omitempty"`
	Arch       string            `json:"arch,omitempty"`
	Token      string            `json:"token,omitempty"`
	Targets    map[string]string `json:"targets,omitempty"`
	Target     string            `json:"target,omitempty"`
	HTTP       int               `json:"http,omitempty"`
	Enrollment int               `json:"enrollment,omitempty"`
	Database   int               `json:"database,omitempty"`
	Port       int               `json:"port,omitempty"`
	NoService  bool              `json:"no_service,omitempty"`
	// StorageProbeMS asks a prepared helper to measure synchronous writes for
	// this long (action "storage").
	StorageProbeMS int `json:"storage_probe_ms,omitempty"`
}
type clusterCheckResponse struct {
	Error   string               `json:"error,omitempty"`
	Checks  map[string]string    `json:"checks,omitempty"`
	Storage *clusterCheckStorage `json:"storage,omitempty"`
}
type clusterCheckSession interface {
	Exchange(clusterCheckRequest) (clusterCheckResponse, error)
	Close() error
}
type clusterCheckStarter func(context.Context, clusterInstallNode, []byte) (clusterCheckSession, error)

func clusterCheckCommand(ctx context.Context, args []string, output io.Writer) error {
	o, err := parseClusterOptions(args, output, true)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	if err := resolveClusterInstallSSH(ctx, &o); err != nil {
		return err
	}
	return checkCluster(ctx, o, output)
}

func checkCluster(ctx context.Context, o clusterInstallOptions, output io.Writer) error {
	return clusterCheckWithStarter(ctx, o, output, startClusterCheckSSH)
}

func clusterCheckWithStarter(ctx context.Context, o clusterInstallOptions, output io.Writer, start clusterCheckStarter) error {
	_, err := clusterCheckMeasure(ctx, o, output, start)
	return err
}

// clusterCheckMeasure runs the whole check and returns what its storage
// section measured, which the installer chooses the storage tier from.
func clusterCheckMeasure(ctx context.Context, o clusterInstallOptions, output io.Writer, start clusterCheckStarter) (verdict clusterStorageVerdict, result error) {
	verdict.members = len(o.Hosts)
	progress, _ := output.(*clusterInstallProgress)
	if err := ctx.Err(); err != nil {
		return verdict, err
	}
	binary, err := os.ReadFile(o.Binary)
	if err != nil {
		return verdict, fmt.Errorf("read check executable: %w", err)
	}
	file, err := elf.Open(o.Binary)
	if err != nil {
		return verdict, fmt.Errorf("--binary must be a Linux native dqlite ELF executable: %w", err)
	}
	arch := ""
	switch file.Machine {
	case elf.EM_X86_64:
		arch = "amd64"
	case elf.EM_AARCH64:
		arch = "arm64"
	}
	file.Close()
	if arch == "" {
		return verdict, errors.New("gpuworks cluster check supports Linux amd64 or arm64 native dqlite executables")
	}
	tokens := make(map[string]string, len(o.Hosts))
	for _, host := range o.Hosts {
		challenge := make([]byte, 32)
		if _, err := rand.Read(challenge); err != nil {
			return verdict, err
		}
		tokens[host] = hex.EncodeToString(challenge)
	}
	fmt.Fprintln(output, "\n集群预检查  [✓] 通过  [✗] 失败  [–] 未检查/跳过")
	fmt.Fprintf(output, "节点：%d  HTTP=%d  注册=%d  数据库=%d  成员会话=%d\n", len(o.Hosts), o.HTTP, o.Enrollment, o.Database, o.Database+1)
	sessions := make(map[string]clusterCheckSession)
	managementPassed, peerPassed := 0, 0
	defer func() {
		fmt.Fprintln(output, "\n临时资源清理")
		for _, host := range o.Hosts {
			node := o.node(host)
			if session := sessions[host]; session != nil {
				progress.Task(clusterInstallTask(o, host, "清理预检查临时资源"))
				err := session.Close()
				clusterCheckResultLine(output, node.Name+"：检查程序、临时文件与监听端口", err)
				if err != nil {
					result = errors.Join(result, fmt.Errorf("%s: release check helper: %w", host, err))
				}
			} else {
				clusterCheckLine(output, "–", node.Name, "未建立检查会话")
			}
		}
		if result == nil {
			fmt.Fprintf(output, "\n预检查通过：%d/%d 节点，%d/%d 管理机访问路径，%d/%d 节点间路径；临时资源已清理。\n", len(o.Hosts), len(o.Hosts), managementPassed, len(o.Hosts), peerPassed, len(o.Hosts)*(len(o.Hosts)-1)*3)
		} else {
			if err := ctx.Err(); err != nil {
				clusterCheckResultLine(output, "检查中断", err)
			}
			fmt.Fprintln(output, "\n预检查未通过：请处理 [✗] 项后重新检查；[–] 项尚未验证。")
		}
	}()
	var failures []error
	for _, host := range o.Hosts {
		if err := ctx.Err(); err != nil {
			clusterCheckLine(output, "–", "其余节点及网络连通性", "检查中断，未检查")
			return verdict, errors.Join(append(failures, err)...)
		}
		node := o.node(host)
		progress.Task(clusterInstallTask(o, host, "SSH 登录、传输检查程序并检查节点条件"))
		fmt.Fprintf(output, "\n%s (%s)\n", node.Name, host)
		fmt.Fprintln(output, "  正在连接 SSH 并传输检查程序…")
		session, err := start(ctx, node, binary)
		clusterCheckResultLine(output, "SSH 登录与检查程序传输", err)
		if err != nil {
			// SSH already failed. Print the unchecked rows; their missing-result error is the same failure.
			_ = clusterCheckPreparationLines(output, node, o, nil)
			failures = append(failures, fmt.Errorf("%s (%s): SSH/copy preflight: %w", node.Name, host, err))
			continue
		}
		sessions[host] = session
		response, err := session.Exchange(clusterCheckRequest{Action: "prepare", Dir: node.Dir, Host: host, User: node.SSH.User, Arch: arch, Token: tokens[host], Targets: tokens, HTTP: o.HTTP, Enrollment: o.Enrollment, Database: o.Database, NoService: o.NoService})
		if err == nil && response.Error != "" {
			err = errors.New(response.Error)
		}
		reportErr := clusterCheckPreparationLines(output, node, o, response.Checks)
		if err == nil {
			err = reportErr
		}
		if err != nil {
			clusterCheckResultLine(output, "节点准备", err)
			failures = append(failures, fmt.Errorf("%s (%s): node preflight: %w", node.Name, host, err))
		}
	}
	// Every node must hold all its listeners before checking paths. An incomplete
	// preparation is never reported as a directional network failure.
	if len(failures) != 0 {
		fmt.Fprintln(output, "\n网络连通性")
		clusterCheckLine(output, "–", "管理机 → 节点 HTTP 端口", "节点准备未全部通过")
		clusterCheckLine(output, "–", "节点间注册 / 数据库 / 成员会话端口", "节点准备未全部通过；未进行互通检查")
		return verdict, errors.Join(failures...)
	}
	fmt.Fprintln(output, "\n管理机 → 节点 HTTP 端口（TCP 连通性）")
	for _, host := range o.Hosts {
		if err := ctx.Err(); err != nil {
			clusterCheckLine(output, "–", "其余管理机访问与节点间路径", "检查中断，未检查")
			return verdict, errors.Join(append(failures, err)...)
		}
		progress.Task(clusterInstallTask(o, host, "检查管理机到节点 HTTP 端口"))
		err := clusterCheckProbe(ctx, host, o.HTTP, tokens[host])
		clusterCheckResultLine(output, fmt.Sprintf("管理机 → %s (%s) HTTP TCP:%d", o.node(host).Name, host, o.HTTP), err)
		if err != nil {
			failures = append(failures, fmt.Errorf("management -> %s HTTP:%d: %w", host, o.HTTP, err))
		} else {
			managementPassed++
		}
	}
	fmt.Fprintln(output, "\n节点间端口互通（两个方向分别检查）")
	for _, source := range o.Hosts {
		for _, target := range o.Hosts {
			if source == target {
				continue
			}
			fmt.Fprintf(output, "  %s (%s) → %s (%s)\n", o.node(source).Name, source, o.node(target).Name, target)
			for _, port := range []int{o.Enrollment, o.Database, o.Database + 1} {
				if err := ctx.Err(); err != nil {
					clusterCheckLine(output, "–", "其余节点间路径", "检查中断，未检查")
					return verdict, errors.Join(append(failures, err)...)
				}
				progress.Task(fmt.Sprintf("%s → %s：检查 %s TCP:%d", source, target, clusterCheckPortName(o, port), port))
				response, err := sessions[source].Exchange(clusterCheckRequest{Action: "probe", Target: target, Port: port, Token: tokens[target]})
				if err == nil && response.Error != "" {
					err = errors.New(response.Error)
				}
				fmt.Fprint(output, "  ")
				clusterCheckResultLine(output, fmt.Sprintf("%s TCP:%d", clusterCheckPortName(o, port), port), err)
				if err != nil {
					failures = append(failures, fmt.Errorf("%s -> %s TCP:%d: %w", source, target, port, err))
				} else {
					peerPassed++
				}
			}
		}
	}
	if len(failures) != 0 {
		return verdict, errors.Join(failures...)
	}
	progress.Task("测量各节点存储同步写延迟")
	verdict = clusterCheckStorageSection(ctx, o, output, sessions)
	return verdict, nil
}

const clusterCheckProbeTimeout = 5 * time.Second
