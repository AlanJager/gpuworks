package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

type reportedClusterCheckSession struct {
	*fakeClusterCheckSession
	prepare func(*clusterCheckResponse)
}

func (s *reportedClusterCheckSession) Exchange(req clusterCheckRequest) (clusterCheckResponse, error) {
	response, err := s.fakeClusterCheckSession.Exchange(req)
	if req.Action == "prepare" && s.prepare != nil {
		s.prepare(&response)
	}
	return response, err
}

func TestClusterCheckChecklistReflectsResultsAndCleanup(t *testing.T) {
	for _, scenario := range []string{"success", "no-service", "occupied-port", "missing-results", "directory", "ssh", "direction", "cleanup", "cancel-before-management"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ports := checkPorts(t, "127.0.0.1")
			o := clusterInstallOptions{Hosts: []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}, Binary: executable, HTTP: ports[0], Enrollment: ports[1], Database: ports[2], Dir: "/tmp/new", NoService: scenario == "no-service"}
			var output bytes.Buffer
			var sessions []*reportedClusterCheckSession
			start := func(_ context.Context, node clusterInstallNode, _ []byte) (clusterCheckSession, error) {
				if scenario == "ssh" && node.Address == o.Hosts[0] {
					return nil, errors.New("SSH denied")
				}
				s := &reportedClusterCheckSession{fakeClusterCheckSession: &fakeClusterCheckSession{host: node.Address, port: o.HTTP}}
				if node.Address == o.Hosts[0] {
					switch scenario {
					case "occupied-port":
						s.prepare = func(r *clusterCheckResponse) {
							r.Checks[fmt.Sprintf("port:%d", o.Database+1)] = "bind: address already in use"
							r.Error = "bind: address already in use"
						}
					case "missing-results":
						s.prepare = func(r *clusterCheckResponse) { r.Checks = nil }
					case "directory":
						s.prepare = func(r *clusterCheckResponse) {
							r.Checks = map[string]string{"runtime": "", "account": "", "directory": "installation directory already exists"}
							r.Error = "installation directory already exists"
						}
					case "direction":
						s.fail = o.Hosts[2]
					case "cleanup":
						s.closeError = errors.New("owned scratch retained")
					}
				}
				if scenario == "cancel-before-management" && node.Address == o.Hosts[2] {
					s.prepare = func(*clusterCheckResponse) { cancel() }
				}
				sessions = append(sessions, s)
				return s, nil
			}
			err = clusterCheckWithStarter(ctx, o, &output, start)
			text := output.String()
			passed := scenario == "success" || scenario == "no-service"
			if (err == nil) != passed || strings.Contains(text, "预检查通过：") != passed {
				t.Fatalf("checklist disagrees with result: %v\n%s", err, text)
			}
			for _, s := range sessions {
				if !s.closed {
					t.Fatal("checklist returned before cleanup")
				}
			}
			assertContains := func(want string) {
				t.Helper()
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q:\n%s", want, text)
				}
			}
			assertContains("临时资源清理")
			switch scenario {
			case "success", "no-service":
				assertContains("3/3 节点，3/3 管理机访问路径，18/18 节点间路径")
				if strings.Count(text, "[✓] 端口空闲") != 12 || strings.Count(text, "[✓] 管理机 →") != 3 {
					t.Fatalf("missing node ports or management routes:\n%s", text)
				}
				for _, port := range []int{o.Enrollment, o.Database, o.Database + 1} {
					want := fmt.Sprintf("[✓] %s TCP:%d", clusterCheckPortName(o, port), port)
					if strings.Count(text, want) != 6 {
						t.Fatalf("missing directional routes for %d:\n%s", port, text)
					}
				}
				if scenario == "no-service" {
					assertContains("[–] 系统服务（systemd / sudo -n / 服务名未占用）：--no-service，无需检查")
				}
			case "occupied-port":
				assertContains(fmt.Sprintf("[✗] 端口空闲 成员会话 TCP:%d（本机绑定）：bind: address already in use", o.Database+1))
			case "missing-results":
				assertContains("[–] 运行环境")
				assertContains("missing check result: runtime")
			case "directory":
				assertContains("[✗] 安装目录不存在、sudo 可创建并设置权限：/tmp/new：installation directory already exists")
				assertContains("[–] 端口空闲")
			case "ssh":
				assertContains("[✗] SSH 登录与检查程序传输：SSH denied")
				assertContains("[–] 运行环境")
			case "direction":
				assertContains("127.0.0.1) →")
				assertContains(fmt.Sprintf("[✗] 数据库 TCP:%d：injected firewall failure", o.Database))
				assertContains(fmt.Sprintf("[✓] 数据库 TCP:%d", o.Database))
			case "cleanup":
				assertContains("检查程序、临时文件与监听端口：owned scratch retained")
			case "cancel-before-management":
				assertContains("[–] 其余管理机访问与节点间路径：检查中断，未检查")
				if !errors.Is(err, context.Canceled) || strings.Contains(text, "[✗] 管理机 →") || strings.Contains(text, "[✓] 管理机 →") {
					t.Fatalf("unattempted paths were reported as checked: %v\n%s", err, text)
				}
			}
			if scenario == "occupied-port" || scenario == "missing-results" || scenario == "directory" || scenario == "ssh" {
				assertContains("未进行互通检查")
				for _, s := range sessions {
					if len(s.requests) != 1 {
						t.Fatal("probed despite incomplete preparation")
					}
				}
			}
		})
	}
}
