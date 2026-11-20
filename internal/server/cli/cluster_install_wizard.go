package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

func runClusterInstallWizard(ctx context.Context, args []string, streams *selfinstall.IO, execute func(context.Context, []string, bool) error) error {
	if !streams.Terminal() {
		return fmt.Errorf("interactive cluster configuration requires a terminal; use gpuworks cluster install --config FILE --password-file FILE for scripts: %w", selfinstall.ErrNoTerminal)
	}
	var options clusterInstallOptions
	flags := clusterInstallFlagSet(&options, streams.Out, false)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("gpuworks cluster install does not accept positional arguments")
	}
	// Short-form host settings are kept for scripts; the wizard edits a config.
	var shortFlag string
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "hosts", "ssh-user", "ssh-key", "dir":
			shortFlag = f.Name
		}
	})
	if shortFlag != "" {
		return fmt.Errorf("interactive configuration cannot use --%s; use --config to edit an existing file", shortFlag)
	}
	prompts := &selfinstall.IO{In: clusterWizardEOFReader{streams.In}, Out: streams.Out, Err: streams.Err, IsTerminal: func() bool { return true }}
	prompts.Say("GPUWorks 三节点安装引导。先选择 SSH 配置方式，再填写三个集群 IP。")
	prompts.Say("配置不包含管理员密码。先核对三台 SSH 主机密钥；保存后可选择仅保存、预检查或安装。")
	defaultPath := flags.Lookup("config").Value.String()
	if defaultPath == "" {
		defaultPath = "cluster.yaml"
	}
	path, err := clusterWizardLocalPath(defaultPath)
	if err != nil {
		return err
	}
	prompts.Say("配置文件：%s（只保存这一份配置；可用 --config 指定路径）。", path)
	var original []byte
	var previous os.FileInfo
	if previous, err = os.Lstat(path); err == nil {
		if !previous.Mode().IsRegular() {
			return errors.New("configuration path must be a regular file, not a symlink or directory")
		}
		loaded := clusterInstallOptions{HTTP: 8800, Enrollment: 9443, Database: 9001, NoService: options.NoService}
		if err := readClusterInstallConfig(path, &loaded, nil); err != nil {
			return fmt.Errorf("existing configuration was retained; choose another --config path or correct it: %w", err)
		}
		if err := validateClusterInstallHosts(&loaded); err != nil {
			return err
		}
		if err := validateClusterInstallPorts(loaded.HTTP, loaded.Enrollment, loaded.Database); err != nil {
			return err
		}
		original, err = os.ReadFile(path)
		if err != nil {
			return err
		}
		options.Nodes = loaded.Nodes
		for _, name := range []string{"http-port", "enrollment-port", "database-port"} {
			set := false
			flags.Visit(func(f *flag.Flag) { set = set || f.Name == name })
			if !set {
				switch name {
				case "http-port":
					options.HTTP = loaded.HTTP
				case "enrollment-port":
					options.Enrollment = loaded.Enrollment
				case "database-port":
					options.Database = loaded.Database
				}
			}
		}
		prompts.Say("已载入 %s，逐台设置将作为默认值。", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	config, err := collectClusterWizardConfig(ctx, prompts, options)
	if err != nil {
		return err
	}
	raw, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	prompts.Say("配置将保存到 %s（0600），不包含管理员密码。", path)
	confirmed, err := clusterWizardConfirm(ctx, prompts, "保存以上配置？", true)
	if err != nil || !confirmed {
		return err
	}
	if previous != nil {
		confirmed, err = clusterWizardConfirm(ctx, prompts, "覆盖已有配置文件？", false)
		if err != nil || !confirmed {
			return err
		}
	}
	if err := saveClusterWizardConfig(ctx, path, raw, previous, original); err != nil {
		return err
	}
	prompts.Say("已保存 %s（0600）。", path)
	binaryHint := "--binary /absolute/path/to/gpuworks-ha"
	if options.Binary != "" {
		binaryHint = "--binary " + clusterInstallQuote(options.Binary)
	}
	prompts.Say("预检查命令：gpuworks cluster check --config %s %s", clusterInstallQuote(path), binaryHint)
	if flags.Lookup("configure").Value.String() == "true" {
		return nil
	}
	choice, err := clusterWizardValue(ctx, prompts, "下一步：1 仅保存，2 预检查，3 开始安装", "1", func(s string) error {
		if s != "1" && s != "2" && s != "3" {
			return errors.New("请输入 1、2 或 3")
		}
		return nil
	})
	if err != nil || choice == "1" {
		return err
	}
	if options.Binary == "" {
		return errors.New("configuration saved; --binary is required for preflight or installation: select a compatible Linux HA gpuworks binary")
	}
	next := []string{"--config", path, "--timeout", options.Timeout.String()}
	if options.Binary != "" {
		next = append(next, "--binary", options.Binary)
	}
	if options.NoService {
		next = append(next, "--no-service")
	}
	if choice == "3" {
		password, err := clusterWizardPassword(ctx, prompts, streams, flags.Lookup("password-file").Value.String(), filepath.Dir(path))
		if err != nil {
			return err
		}
		next = append(next, "--password-file", password)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return execute(ctx, next, choice == "2")
}

func collectClusterWizardConfig(ctx context.Context, prompts *selfinstall.IO, options clusterInstallOptions) (clusterInstallConfig, error) {
	config := clusterInstallConfig{Version: 1}
	config.Ports.HTTP, config.Ports.Enrollment, config.Ports.Database = options.HTTP, options.Enrollment, options.Database
	if err := validateClusterInstallPorts(options.HTTP, options.Enrollment, options.Database); err != nil {
		return config, err
	}
	mode, err := clusterWizardValue(ctx, prompts, "SSH 配置方式：1 沿用已有免密设置（不再填写），2 三台通用，3 逐节点", "1", func(s string) error {
		if s != "1" && s != "2" && s != "3" {
			return errors.New("请输入 1、2 或 3")
		}
		return nil
	})
	if err != nil {
		return config, err
	}
	// A loaded directory is always explicit, even if it matches an older default.
	var preserveDir [3]bool
	for i := range 3 {
		node := clusterInstallNode{Name: fmt.Sprintf("control-%d", i+1)}
		if len(options.Nodes) == 3 {
			node = options.Nodes[i]
			preserveDir[i] = node.Dir != ""
		}
		oldAddress := node.Address
		address, err := clusterWizardValue(ctx, prompts, fmt.Sprintf("节点 %d 集群 IPv4", i+1), node.Address, func(s string) error {
			ip := net.ParseIP(s)
			if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() {
				return errors.New("请输入可达的非回环 IPv4 地址")
			}
			for _, existing := range config.Nodes {
				if existing.Address == ip.String() {
					return errors.New("集群 IPv4 不能重复")
				}
			}
			return nil
		})
		if err != nil {
			return config, err
		}
		node.Address = net.ParseIP(address).String()
		if node.SSH.Target == "" || node.SSH.Target == oldAddress {
			node.SSH.Target = node.Address
		}
		config.Nodes = append(config.Nodes, node)
	}
	if mode == "2" {
		common := config.Nodes[0].SSH
		common, err = clusterWizardSSH(ctx, prompts, "三台通用", common, false)
		if err != nil {
			return config, err
		}
		for i := range config.Nodes {
			node := &config.Nodes[i]
			node.SSH.User, node.SSH.Port, node.SSH.Key = common.User, common.Port, common.Key
			updateClusterWizardDefaultDir(ctx, node, options.NoService, preserveDir[i])
		}
	} else if mode == "3" {
		for i := range config.Nodes {
			node := &config.Nodes[i]
			ssh, err := clusterWizardSSH(ctx, prompts, fmt.Sprintf("节点 %d", i+1), node.SSH, true)
			if err != nil {
				return config, err
			}
			node.SSH = ssh
			updateClusterWizardDefaultDir(ctx, node, options.NoService, preserveDir[i])
		}
	}
	for {
		if err := showClusterWizardSettings(ctx, prompts, &config, options.NoService); err != nil {
			return config, err
		}
		choice, err := clusterWizardValue(ctx, prompts, "设置：0 使用以上设置，1 修改节点1，2 修改节点2，3 修改节点3，4 修改集群端口", "0", func(s string) error {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 || n > 4 {
				return errors.New("请输入 0、1、2、3 或 4")
			}
			return nil
		})
		if err != nil {
			return config, err
		}
		if choice == "0" {
			return config, nil
		}
		if choice == "4" {
			if err := editClusterWizardPorts(ctx, prompts, &config); err != nil {
				return config, err
			}
			continue
		}
		index, _ := strconv.Atoi(choice)
		node := &config.Nodes[index-1]
		ssh, err := clusterWizardSSH(ctx, prompts, fmt.Sprintf("节点 %d", index), node.SSH, true)
		if err != nil {
			return config, err
		}
		node.SSH = ssh
		updateClusterWizardDefaultDir(ctx, node, options.NoService, preserveDir[index-1])
		suggestedDir := node.Dir
		node.Dir, err = clusterWizardValue(ctx, prompts, fmt.Sprintf("节点 %d 安装目录", index), suggestedDir, validateClusterInstallDir)
		preserveDir[index-1] = preserveDir[index-1] || node.Dir != suggestedDir
		if err != nil {
			return config, err
		}
	}
}

func updateClusterWizardDefaultDir(ctx context.Context, node *clusterInstallNode, noService, preserve bool) {
	if !preserve {
		user, _, _ := clusterInstallSSHConfig(ctx, *node)
		node.Dir = clusterInstallDefaultDir(user, noService)
	}
}

func showClusterWizardSettings(ctx context.Context, prompts *selfinstall.IO, config *clusterInstallConfig, noService bool) error {
	prompts.Say("\n沿用本机 SSH config/agent；这里只读取设置，尚未验证免密登录。")
	for i := range config.Nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		node := &config.Nodes[i]
		user, port, err := clusterInstallSSHConfig(ctx, *node)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			prompts.Warn("%s：无法读取本机 SSH 设置，可选择修改该节点：%v", node.Name, err)
		}
		if node.Dir == "" {
			node.Dir = clusterInstallDefaultDir(user, noService)
		}
		identity := "自动（SSH config/agent）"
		if node.SSH.Key != "" {
			identity = node.SSH.Key
		}
		connection := "未识别（请核对）"
		if err == nil {
			connection = fmt.Sprintf("%s@%s:%d", user, node.SSH.Target, port)
		}
		prompts.Say("%s  集群IP=%s  SSH=%s\n  密钥=%s  安装目录=%s", node.Name, node.Address, connection, identity, node.Dir)
	}
	prompts.Say("集群端口：HTTP=%d  Enrollment=%d  Database=%d  成员会话=%d", config.Ports.HTTP, config.Ports.Enrollment, config.Ports.Database, config.Ports.Database+1)
	return nil
}

func editClusterWizardPorts(ctx context.Context, prompts *selfinstall.IO, config *clusterInstallConfig) error {
	for {
		for _, field := range []struct {
			label string
			value *int
		}{{"HTTP 端口", &config.Ports.HTTP}, {"Enrollment 端口", &config.Ports.Enrollment}, {"Database 端口（成员会话使用下一端口）", &config.Ports.Database}} {
			value, err := clusterWizardValue(ctx, prompts, field.label, strconv.Itoa(*field.value), func(s string) error {
				n, err := strconv.Atoi(s)
				if err != nil || n < 1024 || n > 65535 {
					return errors.New("端口须为 1024 到 65535 的整数")
				}
				return nil
			})
			if err != nil {
				return err
			}
			*field.value, _ = strconv.Atoi(value)
		}
		if err := validateClusterInstallPorts(config.Ports.HTTP, config.Ports.Enrollment, config.Ports.Database); err != nil {
			prompts.Warn("端口配置无效：%v；请重新输入三个端口。", err)
			continue
		}
		return nil
	}
}

func clusterWizardSSH(ctx context.Context, prompts *selfinstall.IO, label string, ssh clusterInstallSSH, target bool) (clusterInstallSSH, error) {
	var err error
	if target {
		ssh.Target, err = clusterWizardValue(ctx, prompts, label+" SSH 地址或别名", ssh.Target, func(s string) error { return validateClusterInstallSSH(clusterInstallSSH{Target: s}) })
		if err != nil {
			return ssh, err
		}
	}
	defaultUser := ssh.User
	if defaultUser == "" {
		defaultUser, _, _ = clusterInstallSSHConfig(ctx, clusterInstallNode{SSH: ssh})
	}
	user, err := clusterWizardValue(ctx, prompts, label+" SSH 账号（自动沿用 SSH config）", clusterWizardOptional(defaultUser), func(s string) error { return validateClusterInstallSSH(clusterInstallSSH{User: clusterWizardClear(s)}) })
	if err != nil {
		return ssh, err
	}
	ssh.User = clusterWizardClear(user)
	defaultPort := ssh.Port
	if defaultPort == 0 {
		_, defaultPort, _ = clusterInstallSSHConfig(ctx, clusterInstallNode{SSH: ssh})
		if defaultPort == 0 {
			defaultPort = 22
		}
	}
	port, err := clusterWizardValue(ctx, prompts, label+" SSH 端口（自动沿用 SSH config）", strconv.Itoa(defaultPort), func(s string) error {
		if clusterWizardClear(s) == "" {
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("SSH 端口须为 1 到 65535，或输入 自动")
		}
		return nil
	})
	if err != nil {
		return ssh, err
	}
	ssh.Port = 0
	if clusterWizardClear(port) != "" {
		ssh.Port, _ = strconv.Atoi(port)
	}
	defaultKey := ssh.Key
	if defaultKey == "" {
		defaultKey = clusterWizardDefaultKey(ctx, ssh)
	}
	key, err := clusterWizardValue(ctx, prompts, label+" SSH 私钥（自动使用 SSH config/agent）", clusterWizardOptional(defaultKey), func(s string) error {
		if clusterWizardClear(s) == "" {
			return nil
		}
		path, err := clusterWizardLocalPath(s)
		if err != nil {
			return err
		}
		if !clusterWizardReadableKey(path) {
			return errors.New("私钥须是本机可读的常规文件")
		}
		return nil
	})
	if err != nil {
		return ssh, err
	}
	ssh.Key = ""
	if clusterWizardClear(key) != "" {
		ssh.Key, err = clusterWizardLocalPath(key)
	}
	return ssh, err
}

func clusterWizardOptional(value string) string {
	if value == "" {
		return "自动"
	}
	return value
}
func clusterWizardClear(value string) string {
	if value == "-" || value == "自动" {
		return ""
	}
	return value
}
func clusterWizardLocalPath(path string) (string, error) {
	if strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("路径包含非法字符")
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[2:])
	}
	return filepath.Abs(path)
}
