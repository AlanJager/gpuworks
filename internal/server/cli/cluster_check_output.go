package cli

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func clusterCheckLine(output io.Writer, state, label, detail string) {
	fmt.Fprintf(output, "  [%s] %s", state, label)
	if detail != "" {
		fmt.Fprintf(output, "：%s", strings.ReplaceAll(detail, "\n", "\n      "))
	}
	fmt.Fprintln(output)
}

func clusterCheckResultLine(output io.Writer, label string, err error) {
	if err != nil {
		clusterCheckLine(output, "✗", label, err.Error())
		return
	}
	clusterCheckLine(output, "✓", label, "")
}

func clusterCheckPortName(o clusterInstallOptions, port int) string {
	switch port {
	case o.HTTP:
		return "HTTP"
	case o.Enrollment:
		return "注册"
	case o.Database:
		return "数据库"
	default:
		return "成员会话"
	}
}

// Absent results are never inferred as successful from an empty overall error.
func clusterCheckPreparationLines(output io.Writer, node clusterInstallNode, o clusterInstallOptions, checks map[string]string) error {
	directoryLabel := "安装目录不存在、sudo 可创建并设置权限：" + node.Dir
	if o.NoService {
		directoryLabel = "安装目录不存在、父目录可写：" + node.Dir
	}
	items := [][2]string{
		{"runtime", "运行环境（Linux / 架构 / native dqlite / 进程支持）"},
		{"account", "SSH 实际账号与配置一致"},
		{"directory", directoryLabel},
		{"service", "系统服务（systemd / sudo -n / 服务名未占用）"},
	}
	for _, port := range []int{o.HTTP, o.Enrollment, o.Database, o.Database + 1} {
		items = append(items, [2]string{"port:" + strconv.Itoa(port), fmt.Sprintf("端口空闲 %s TCP:%d（本机绑定）", clusterCheckPortName(o, port), port)})
	}
	var failures []error
	for _, item := range items {
		if item[0] == "service" && o.NoService {
			clusterCheckLine(output, "–", item[1], "--no-service，无需检查")
			continue
		}
		detail, checked := checks[item[0]]
		if !checked {
			clusterCheckLine(output, "–", item[1], "未检查")
			failures = append(failures, fmt.Errorf("missing check result: %s", item[0]))
		} else if detail != "" {
			clusterCheckLine(output, "✗", item[1], detail)
			failures = append(failures, errors.New(detail))
		} else {
			clusterCheckLine(output, "✓", item[1], "")
		}
	}
	return errors.Join(failures...)
}
