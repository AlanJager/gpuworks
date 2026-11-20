package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/version"
)

// clusterInstallOptions are transport settings, never persisted credentials.
type clusterInstallOptions struct {
	Hosts                            []string
	Nodes                            []clusterInstallNode
	User, Key, Binary, Dir, Password string
	// DatabaseHelper is the optional local replicated database helper copied
	// beside the server executable on every member.
	DatabaseHelper             string
	HTTP, Enrollment, Database int
	NoService                  bool
	Timeout                    time.Duration
	// StorageProbe is how long each member's synchronous writes are
	// measured before the storage tier is chosen; zero skips it.
	StorageProbe time.Duration
	// StorageTier is the tier the operator named (flag or cluster config);
	// empty lets the installation choose it from the measurement.
	StorageTier storagetier.Tier
}

type clusterInstallNodeRequest struct {
	Action, Dir, Host, User, Password string
	CAFingerprint                     string
	ClusterID                         string
	Hosts                             []string
	HTTP, Enrollment, Database        int
	NoService                         bool
	Invitation                        json.RawMessage
	// StorageTier is the cluster's tier as it is recorded (empty: standard).
	// The seed records it and every joining member must name the same one.
	StorageTier string `json:",omitempty"`
}

type clusterInstallRunner func(context.Context, string, io.Reader) ([]byte, error)

func clusterInstallCommand(ctx context.Context, args []string, output io.Writer) error {
	streams := selfinstall.NewIO()
	streams.Out = output
	return clusterInstallCommandWithIO(ctx, args, streams)
}

func clusterInstallCommandWithIO(ctx context.Context, args []string, streams *selfinstall.IO) (resultErr error) {
	output := streams.Out
	var flagsOptions clusterInstallOptions
	flags := clusterInstallFlagSet(&flagsOptions, output, false)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(args) == 0 || flags.Lookup("interactive").Value.String() == "true" || flags.Lookup("configure").Value.String() == "true" {
		return runClusterInstallWizard(ctx, args, streams, func(ctx context.Context, args []string, check bool) error {
			if check {
				return clusterCheckCommand(ctx, args, output)
			}
			return clusterInstallCommandWithIO(ctx, args, streams)
		})
	}
	o, err := parseClusterInstall(args, output)
	passwordFileSet := false
	flags.Visit(func(f *flag.Flag) { passwordFileSet = passwordFileSet || f.Name == "password-file" })
	if errors.Is(err, errClusterInstallPasswordRequired) && flags.Lookup("config").Value.String() != "" && !passwordFileSet && streams.Terminal() {
		streams.Say("配置已验证。输入管理员密码后开始预检查和安装（密码仅用于本次安装，不写入文件）。")
		o.Password, err = promptClusterInstallPassword(ctx, streams)
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	progress, err := newClusterInstallProgress(output, o.NoService, o.Password)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, ctx.Err())
			fmt.Fprintln(progress, "远端已创建的数据和日志保留；节点详细错误见对应安装目录下的 install-preflight-errors.log、logs/install-node-errors.log、logs/server.log、logs/serve.out、logs/join.out（按已执行步骤生成）。")
			for _, host := range o.Hosts {
				fmt.Fprintf(progress, "  %s — %s\n", host, o.node(host).Dir)
			}
		}
		resultErr = progress.Finish(resultErr)
	}()
	output = progress
	fmt.Fprintf(output, "版本：%s\n", version.String())
	progress.StartStage("预检查")
	progress.Task("解析 SSH 配置")
	if err := resolveClusterInstallSSH(ctx, &o); err != nil {
		return err
	}
	progress.Task("检查三台节点、端口及网络路径")
	verdict, err := clusterCheckMeasure(ctx, o, output, startClusterCheckSSH)
	if err != nil {
		return err
	}
	// Decided before anything is written on a member: the seed records the
	// tier and the other two members join with it.
	tier, notice, err := decideClusterStorageTier(o.StorageTier, verdict)
	if err != nil {
		return err
	}
	fmt.Fprintln(output, notice)
	o.StorageTier = tier
	progress.CompleteStage()
	remote, shell := clusterInstallTransports(o)
	return installClusterWithProgress(ctx, o, output, remote, shell, progress)
}

func parseClusterInstall(args []string, output io.Writer) (clusterInstallOptions, error) {
	return parseClusterOptions(args, output, false)
}

func clusterInstallFlagSet(o *clusterInstallOptions, output io.Writer, check bool) *flag.FlagSet {
	*o = clusterInstallOptions{HTTP: 8800, Enrollment: 9443, Database: 9001, Timeout: 15 * time.Minute}
	name := "gpuworks cluster install"
	if check {
		name = "gpuworks cluster check"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(output)
	fs.String("hosts", "", "exactly three distinct IPv4 addresses, comma separated")
	fs.String("config", "", "version 1 YAML config with per-node SSH and directory settings")
	fs.StringVar(&o.User, "ssh-user", "", "SSH account on all three hosts")
	fs.StringVar(&o.Key, "ssh-key", "", "local SSH identity file (default: SSH agent/config)")
	fs.StringVar(&o.Binary, "binary", "", "required compatible Linux HA binary with cluster-install-node and cluster-check-node helpers")
	fs.StringVar(&o.DatabaseHelper, "database-helper", "", "optional Linux replicated database helper installed beside the server executable as "+clusterDatabaseHelperName)
	fs.StringVar(&o.Dir, "dir", "", "absent installation directory (default: /opt/gpuworks/server; --no-service: SSH user's home)")
	fs.String("password-file", "", "private initial admin password file (with --config in a terminal: prompt when omitted)")
	fs.IntVar(&o.HTTP, "http-port", o.HTTP, "HTTP port on all hosts")
	fs.IntVar(&o.Enrollment, "enrollment-port", o.Enrollment, "HTTPS enrollment port on all hosts")
	fs.IntVar(&o.Database, "database-port", o.Database, "native mTLS database port on all hosts (member sessions use the next port)")
	fs.DurationVar(&o.Timeout, "timeout", o.Timeout, "overall installation deadline")
	fs.BoolVar(&o.NoService, "no-service", false, "preview only: detached processes without restart after reboot")
	o.StorageProbe = clusterCheckStorageDefault
	if check {
		fs.DurationVar(&o.StorageProbe, "storage-probe", o.StorageProbe, "measure each node's synchronous write latency for this long and report the storage tier it calls for (advisory; 0 skips, at most "+clusterCheckStorageLimit.String()+")")
	}
	if !check {
		fs.DurationVar(&o.StorageProbe, "storage-probe", o.StorageProbe, "measure each node's synchronous write latency for this long and choose the storage tier from it (0 skips, at most "+clusterCheckStorageLimit.String()+")")
		fs.String("storage-tier", "", "timing profile of the cluster: standard or slow-storage (default: chosen from the storage measurement)")
		fs.Bool("interactive", false, "guide configuration interactively (requires a terminal)")
		fs.Bool("configure", false, "interactively write cluster.yaml without remote operations")
	}
	return fs
}

func parseClusterOptions(args []string, output io.Writer, check bool) (clusterInstallOptions, error) {
	var o clusterInstallOptions
	var err error
	fs := clusterInstallFlagSet(&o, output, check)
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() != 0 {
		return o, fmt.Errorf("%s does not accept positional arguments", fs.Name())
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if configFile := fs.Lookup("config").Value.String(); configFile != "" {
		if err := readClusterInstallConfig(configFile, &o, explicit); err != nil {
			return o, err
		}
	} else {
		o.Hosts = strings.Split(fs.Lookup("hosts").Value.String(), ",")
		if !clusterInstallUserPattern.MatchString(o.User) {
			return o, errors.New("--ssh-user requires a Unix account name")
		}
		if o.Dir == "" {
			o.Dir = clusterInstallDefaultDir(o.User, o.NoService)
		}
		if err := validateClusterInstallDir(o.Dir); err != nil {
			return o, err
		}
	}
	if err := validateClusterInstallHosts(&o); err != nil {
		return o, err
	}
	if o.Timeout <= 0 {
		return o, errors.New("--timeout must be positive")
	}
	if err := validateClusterInstallPorts(o.HTTP, o.Enrollment, o.Database); err != nil {
		return o, err
	}
	if check && explicit["password-file"] {
		return o, errors.New("gpuworks cluster check does not use --password-file")
	}
	if o.StorageProbe < 0 || o.StorageProbe > clusterCheckStorageLimit {
		return o, fmt.Errorf("--storage-probe must be between 0 and %s", clusterCheckStorageLimit)
	}
	if !check && explicit["storage-tier"] {
		// The flag overrides the cluster config, as the port flags do.
		if o.StorageTier, err = parseStorageTierFlag(fs.Lookup("storage-tier").Value.String()); err != nil {
			return o, err
		}
		if o.StorageTier == "" {
			return o, errors.New("--storage-tier requires standard or slow-storage")
		}
	}
	if o.Binary == "" {
		return o, errors.New("--binary is required: select a compatible Linux HA gpuworks binary; this build does not include the HA node helpers")
	}
	o.Binary, err = filepath.Abs(o.Binary)
	if err != nil {
		return o, err
	}
	if o.DatabaseHelper != "" {
		if check {
			return o, errors.New("gpuworks cluster check does not use --database-helper")
		}
		o.DatabaseHelper, err = filepath.Abs(o.DatabaseHelper)
		if err != nil {
			return o, err
		}
	}
	// Return the missing-password sentinel only after every other local option
	// has been resolved, so a terminal can supply it without reparsing config.
	if !check {
		if err := readClusterInstallPassword(fs.Lookup("password-file").Value.String(), &o); err != nil {
			return o, err
		}
	}
	return o, nil
}

var errClusterInstallPasswordRequired = errors.New("--password-file is required for non-interactive installation; run gpuworks cluster install --config FILE in a terminal to enter the password (passwords are never command-line arguments)")

func readClusterInstallPassword(path string, o *clusterInstallOptions) error {
	if path == "" {
		return errClusterInstallPasswordRequired
	}
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 {
		return errors.New("password file must be a private regular file (chmod 600)")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	owner, owned := info.Sys().(*syscall.Stat_t)
	if !os.SameFile(before, info) || !owned || owner.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("password file must be a private regular file (chmod 600)")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return err
	}
	o.Password = strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if len(raw) > 4096 || len(o.Password) < 8 || strings.ContainsAny(o.Password, "\r\n\x00") {
		return errors.New("password must be a single line with at least eight bytes")
	}
	return nil
}

func validateClusterInstallDir(dir string) error {
	if !regexp.MustCompile(`^/[a-zA-Z0-9._/-]+$`).MatchString(dir) || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" || filepath.Dir(dir) == "/" || strings.ContainsAny(dir, "\x00\r\n") {
		return errors.New("--dir must name a clean, absent absolute directory using letters, digits, dots, underscores and hyphens")
	}
	return nil
}

func clusterInstallQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
func clusterInstallUnit(dir string) string {
	hash := sha256.Sum256([]byte(dir))
	return "gpuworks-cluster-" + hex.EncodeToString(hash[:8]) + ".service"
}

func installCluster(ctx context.Context, o clusterInstallOptions, output io.Writer, remote clusterInstallRunner, shell func(context.Context, string, string, io.Reader) error) error {
	return installClusterWithProgress(ctx, o, output, remote, shell, nil)
}

func installClusterWithProgress(ctx context.Context, o clusterInstallOptions, output io.Writer, remote clusterInstallRunner, shell func(context.Context, string, string, io.Reader) error, progress *clusterInstallProgress) error {
	progress.StartStage("分发并准备程序")
	progress.Task("读取本地二进制并计算 SHA-256")
	// Validate all machines before creating any installation. SSH authenticates
	// the copied executable; the digest catches partial or altered transfers.
	binary, err := os.ReadFile(o.Binary)
	if err != nil {
		return err
	}
	if len(binary) < 20 || !bytes.Equal(binary[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return errors.New("--binary must be a Linux ELF native dqlite gpuworks executable")
	}
	digest := sha256.Sum256(binary)
	executables := []clusterInstallExecutable{{Name: "gpuworks", Payload: binary, Digest: hex.EncodeToString(digest[:])}}
	if o.DatabaseHelper != "" {
		progress.Task("读取本地数据库辅助程序并计算 SHA-256")
		helper, err := readClusterInstallExecutable(o.DatabaseHelper, "--database-helper")
		if err != nil {
			return err
		}
		helper.Name = clusterDatabaseHelperName
		executables = append(executables, helper)
	}
	for _, host := range o.Hosts {
		node := o.node(host)
		progress.Task(clusterInstallTask(o, host, "复核安装目录及服务条件"))
		preflight := "set -eu; " + clusterInstallDirectoryCommand(node, o.NoService, false)
		if !o.NoService {
			preflight += "; test -d /run/systemd/system; sudo -n true; sudo -n test ! -e " + clusterInstallQuote("/etc/systemd/system/"+clusterInstallUnit(node.Dir)) + "; sudo -n test ! -L " + clusterInstallQuote("/etc/systemd/system/"+clusterInstallUnit(node.Dir))
		}
		if err := shell(ctx, host, preflight, nil); err != nil {
			return err
		}
	}
	for _, host := range o.Hosts {
		node := o.node(host)
		progress.Task(clusterInstallTask(o, host, "传输二进制并校验 SHA-256"))
		for i, executable := range executables {
			// Only the first transfer creates the directory; every transfer is
			// exclusive and verified against the digest computed locally.
			prepare := ""
			if i == 0 {
				prepare = clusterInstallDirectoryCommand(node, o.NoService, true) + "; "
			}
			command := "set -eu; umask 077; " + prepare + clusterInstallExecutableCommand(node.Dir, executable)
			if err := shell(ctx, host, command, bytes.NewReader(executable.Payload)); err != nil {
				return err
			}
		}
	}
	clusterID := ""
	caFingerprint := ""
	invokeWithContext := func(callCtx context.Context, host, action string, invitation json.RawMessage) ([]byte, error) {
		node := o.node(host)
		req := clusterInstallNodeRequest{Action: action, Dir: node.Dir, Host: host, User: node.SSH.User, Password: o.Password, Hosts: o.Hosts, ClusterID: clusterID, CAFingerprint: caFingerprint, HTTP: o.HTTP, Enrollment: o.Enrollment, Database: o.Database, NoService: o.NoService, Invitation: invitation, StorageTier: o.StorageTier.Recorded()}
		if action == "capabilities" {
			req.Password = ""
		}
		raw, _ := json.Marshal(req)
		response, err := remote(callCtx, host, bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", action, err)
		}
		var value map[string]json.RawMessage
		if json.Unmarshal(response, &value) != nil || value == nil {
			return nil, errors.New("remote installation helper returned invalid JSON")
		}
		if action == "status" {
			var report string
			if json.Unmarshal(value["status"], &report) != nil || !strings.HasPrefix(report, "Status report\n") {
				return nil, errors.New("remote status response is missing its report")
			}
		}
		if action == "bootstrap" {
			var ready bool
			if json.Unmarshal(value["bootstrap_ready"], &ready) != nil || !ready {
				return nil, errors.New("remote material bootstrap prerequisite was not verified")
			}
		}
		return response, nil
	}
	invoke := func(host, action string, invitation json.RawMessage) ([]byte, error) {
		progress.Task(clusterInstallTask(o, host, clusterInstallActionLabel(action)))
		return invokeWithContext(ctx, host, action, invitation)
	}
	directJoinedService := true
	for _, host := range o.Hosts {
		response, err := invoke(host, "capabilities", nil)
		if err != nil {
			return err
		}
		var capabilities struct {
			Native              bool   `json:"native_dqlite"`
			OS                  string `json:"os"`
			DirectJoinedService bool   `json:"direct_joined_service"`
		}
		if json.Unmarshal(response, &capabilities) != nil || !capabilities.Native || capabilities.OS != "linux" {
			return errors.New("copied gpuworks executable requires native Linux dqlite support; no data was initialized")
		}
		directJoinedService = directJoinedService && capabilities.DirectJoinedService
	}
	for _, host := range o.Hosts {
		if _, err := invoke(host, "prepare", nil); err != nil {
			return err
		}
	}
	progress.CompleteStage()
	progress.StartStage("初始化首节点")
	seed, err := invoke(o.Hosts[0], "seed", nil)
	if err != nil {
		return err
	}
	var identity struct {
		ClusterID     string `json:"cluster_id"`
		CAFingerprint string `json:"ca_fingerprint"`
	}
	if json.Unmarshal(seed, &identity) != nil || identity.ClusterID == "" || identity.CAFingerprint == "" {
		return errors.New("seed returned no cluster identity")
	}
	clusterID = identity.ClusterID
	caFingerprint = identity.CAFingerprint
	progress.CompleteStage()
	progress.StartStage("加入其余节点")
	for _, host := range o.Hosts[1:] {
		seedNode := o.node(o.Hosts[0])
		req := clusterInstallNodeRequest{Action: "invite", Dir: seedNode.Dir, Host: o.Hosts[0], User: seedNode.SSH.User, Password: o.Password, HTTP: o.HTTP, Enrollment: o.Enrollment, Database: o.Database, NoService: o.NoService, Hosts: o.Hosts, ClusterID: clusterID, CAFingerprint: caFingerprint, Invitation: json.RawMessage(fmt.Sprintf(`{"host":%q}`, host))}
		raw, _ := json.Marshal(req)
		progress.Task(clusterInstallTask(o, host, "向首节点申请加入邀请"))
		invitation, err := remote(ctx, o.Hosts[0], bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("invite for %s: %w", host, err)
		}
		if _, err := invoke(host, "join", invitation); err != nil {
			return err
		}
	}
	progress.CompleteStage()
	progress.StartStage("启动控制面")
	serviceHosts := o.Hosts
	if directJoinedService {
		action := "serve-joined"
		if !o.NoService {
			action = "service"
			serviceHosts = o.Hosts[:1]
		}
		if err := clusterInstallStartJoined(ctx, o, action, invokeWithContext, progress); err != nil {
			return err
		}
	} else {
		for _, host := range o.Hosts[1:] {
			if _, err := invoke(host, "serve-joined", nil); err != nil {
				return err
			}
		}
	}
	if !o.NoService {
		progress.CompleteStage()
		if directJoinedService {
			progress.StartStage("启用首节点系统服务")
		} else {
			progress.StartStage("启用系统服务")
		}
	}
	for _, host := range serviceHosts {
		if _, err := invoke(host, "service", nil); err != nil {
			return err
		}
	}
	progress.CompleteStage()
	progress.StartStage("等待集群同步")
	for _, host := range o.Hosts {
		if _, err := invoke(host, "bootstrap", nil); err != nil {
			return err
		}
	}
	progress.CompleteStage()
	progress.StartStage("验证三节点读写")
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	key := "cluster_install_" + hex.EncodeToString(nonce)
	// Status may converge while starting. Once a host reports ready, its paired
	// business operations run once and any failure is terminal.
	for _, host := range o.Hosts {
		var lastStatus error
		for {
			if _, err := invoke(host, "status", nil); err == nil {
				break
			} else {
				lastStatus = err
			}
			select {
			case <-ctx.Done():
				return errors.Join(fmt.Errorf("status did not become ready on %s: %w", host, ctx.Err()), lastStatus)
			case <-time.After(500 * time.Millisecond):
			}
		}
		if _, err := invoke(host, "business", json.RawMessage(fmt.Sprintf(`{"key":%q,"value":%q}`, key, host))); err != nil {
			return err
		}
	}
	// Prove the final committed setting is shared across all three entries.
	for _, host := range o.Hosts {
		if _, err := invoke(host, "read-setting", json.RawMessage(fmt.Sprintf(`{"key":%q,"value":%q}`, key, o.Hosts[2]))); err != nil {
			return err
		}
	}
	progress.CompleteStage()
	fmt.Fprintln(output, "Three control-plane entries passed status, login, nodes and committed settings verification.")
	for _, host := range o.Hosts {
		node := o.node(host)
		fmt.Fprintf(output, "http://%s:%d — directory: %s\n", host, o.HTTP, node.Dir)
		if !o.NoService {
			fmt.Fprintf(output, "  Enabled service: %s\n", clusterInstallUnit(node.Dir))
		}
	}
	if o.NoService {
		fmt.Fprintln(output, "Preview processes will not restart after reboot (--no-service).")
	}
	return nil
}

func clusterInstallTask(o clusterInstallOptions, host, action string) string {
	for i, candidate := range o.Hosts {
		if candidate == host {
			return fmt.Sprintf("节点 %d/%d %s (%s)：%s", i+1, len(o.Hosts), o.node(host).Name, host, action)
		}
	}
	return host + "：" + action
}

// Only these human labels reach the progress log. Helper requests/responses
// carry passwords and invitation tokens and must never be logged.
func clusterInstallActionLabel(action string) string {
	switch action {
	case "capabilities":
		return "确认 Linux native dqlite 能力"
	case "prepare":
		return "创建私有数据与日志目录"
	case "seed":
		return "初始化管理员和集群身份，等待首节点就绪"
	case "join":
		return "加入集群，等待成员接纳与数据库同步"
	case "serve-joined":
		return "启动控制面并等待登录可用"
	case "service":
		return "应用服务运行模式并等待就绪"
	case "bootstrap":
		return "等待集群配置与材料同步"
	case "status":
		return "等待三投票成员和业务状态就绪"
	case "business":
		return "验证登录、节点列表与设置提交"
	case "read-setting":
		return "验证跨节点读取最终提交的设置"
	default:
		return "执行节点安装操作"
	}
}
