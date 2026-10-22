# 隔离 NFS 故障夹具

入口为 `TestL4StorageModelCacheRecoveryNFS` / `test-l4-storage-model-cache-recovery-nfs` / 同名 GitLab manual job。默认测试不启动容器或连接外部 NFS；显式执行时缺少能力即失败。

```bash
docker build -t gpuworks/nfs-fault-fixture:local test/fixtures/nfs-fault
sudo env GPUWORKS_NFS_FAULT_IMAGE=gpuworks/nfs-fault-fixture:local \
  unshare --mount --propagation private -- \
  make test-l4-storage-model-cache-recovery-nfs
```

远端无需 Go 工具链，可以先以 `CGO_ENABLED=0 go test -tags=l4 -c -o nfsfault.test ./test/l4/nfsfault` 编译，再将测试二进制与 `docker save` 的镜像传到独立目录。在目标机以相同环境变量、私有 mount namespace 执行：

```bash
GPUWORKS_TEST_STRICT=1 GPUWORKS_NFS_FAULT_IMAGE=gpuworks/nfs-fault-fixture:local \
  unshare --mount --propagation private -- \
  ./nfsfault.test -test.v -test.timeout=20m -test.run=TestL4StorageModelCacheRecoveryNFS
```

需要 root、Docker、NFS 客户端、`mount/umount`、`losetup`、`dmsetup`、`mkfs.ext4`、`truncate` 和内核 netem。测试会创建独立的稀疏镜像文件、loop 设备和唯一命名的 device-mapper 映射；只把这一个设备交给 Ganesha 容器。容器内部 ext4 挂载可写以共享测试准备的数据，**NFS export 与客户端挂载均为只读**。不发布宿主机端口，不操作现有 NFS 导出、宿主机防火墙、生产磁盘或全局 page cache。

| 场景 | 注入与断言 |
|---|---|
| blackhole | 容器 eth0 的 netem 100% 丢包；真实停滞至少 120 秒，速率归零，控制 RPC/API 和心跳仍可用 |
| pause | 冻结 Ganesha 容器，确认实际读停滞，再解冻恢复 |
| crash | SIGKILL 容器；保留 export 盘和 NFSv4 recovery 目录，再启动恢复 |
| loss | 50±10ms 延迟、10% 丢包下有实际字节进展，恢复后完成 |
| cancel | 阻塞时撤销持久化 intent，stop RPC 必须超时而不能伪报退出；恢复后确认旧 worker 退出、没有迟到安装，再发新 intent |
| disk_full | 先允许拷贝通过空间检查，再将私有目标 tmpfs 配额降至 16 MiB；验证实际写入 ENOSPC、错误分类和重试 API |
| eio | 只将专用测试块设备切换为 error target；只清除此测试文件的干净 page cache，验证真实 NFS EIO、失败状态和重试 API |

每个场景使用新的源文件 inode，权重约 68 MiB；预先只计算本地生成数据的 SHA256，不读取 NFS 权重暖缓存。成功必须恢复并校验全部权重字节。网络故障只进入 NFS 容器，server/agent 的真实 WebSocket 控制连接走本机独立链路。GPU 探测与未使用的推理运行时由 testenv 提供，**不证明 GPU 推理或跨主机网络性能**。

夹具通过 Store 设置模型和拷贝 intent，再由真实 Hub 同步 desired state；错误恢复走真实 cache retry HTTP API，随后显式同步。它验证协议、实际拷贝、NFS 错误与恢复边界，不替代部署创建、共享所有者调度或浏览器操作的验收。

故障前启动独立 watchdog：控制 pipe EOF 或四分钟期限触发专用块设备、网络与容器恢复。常规断言失败也先恢复故障，再停 agent/server；保留失败容器、镜像文件和日志供检查。成功时按依赖顺序卸载客户端、删除测试容器、卸载 export、删除专用映射、释放 loop、删除目录。失败现场检查完成后，只按输出的精确容器名/设备名/目录清理，禁止全局 prune 或无范围清理。

NFS 容器使用独立 Docker network 和固定地址，重启后必须核对地址不变。Docker 19 的默认 bridge 会重新分配地址；夹具先申请空闲子网，再以显式 `--subnet` 重建自己的空网络，避免该版本拒绝静态 IP。不会修改已有 network。

宿主机没有 `sch_netem` 时，不静默跳过或假装完成丢包测试。可以明确以 `GPUWORKS_NFS_FAULT_NETWORK=link` 选择容器接口断网，并将 `-test.run` 限定为 `^TestL4StorageModelCacheRecoveryNFS$/(blackhole|pause|crash|cancel|disk_full|eio)$`。这只执行六项；`loss` 必须另在支持 netem 的机器运行。全套 Make/CI 门禁仍要求七项全部执行。
