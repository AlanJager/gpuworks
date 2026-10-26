# Strata 初始排队耗时运行时资产

此目录为 Strata 0.1.38 提供轻量 Python 补丁及入口包装器，通过现有推理目录资产机制分发，不携带完整引擎源码、框架、权重或镜像层。

`strata_queue.py` 校验原始 `server.py` SHA256，仅以精确行号和上下文应用 `initial-queue.patch`，再校验补丁结果并原子替换容器内的文件。替换不会修改 udocker 可能共享的原始硬链接 inode。已经打过同一补丁时无需写入；未知版本、损坏资产、只读源文件/文件系统会阻止启动并输出诊断。最后通过 `os.execv` 原样转交 argv 给 `/opt/strata/gpuworks-entrypoint.sh`。资产目录可只读挂载，写入只发生在容器自身的 `/opt/strata/serve/server.py`。

补丁返回同一请求回复的 `timings.queue_ms`：初次模型加载前的 FIFO 等待，加首次生成的 FIFO 等待。首次生成准入后冻结，排除模型加载本身、Prefill、Decode 和后续 MCP 轮次的等待。后续轮次可能在首段正文已经可见后发生，不能放进首字前时间条。没有成功 timing 事件的错误仍为未知。可选 `api_monitor` 保持关闭，不保留请求或回复正文。

上游：[Niko1221/Strata v0.1.38](https://github.com/Niko1221/Strata/blob/09817da3979abfeb5fbdef7c8f1223d120c1ad39/serve/server.py)，MIT 许可见 `LICENSE.txt`。

- 原始源文件 SHA256：`fc85cfd3a5b0379c8482de7a81673cae76299751aa5fe614413bc01f1fa54408`
- 补丁后 SHA256：`7f0ddfd4a07181e37a34e61a8b4587e3a97ba582aff8250747f918e93d7fa5cd`
- 固定基础镜像：`swr.cn-southwest-2.myhuaweicloud.com/gpuworks/strata@sha256:91e52d2b90644025682f5c35d45f7c13d1471c31ad32274f32af7646f37f354e`

## 离线单测和确定性打包

仅需 Python 标准库，不依赖外部源码、GPU、镜像或网络。默认 9 项测试全部执行，无跳过：

```sh
python3 -m unittest discover -s build/strata-queue -p 'test_*.py' -v
python3 build/strata-queue/build.py --check
```

修改资产输入后用 `python3 build/strata-queue/build.py` 重新生成模型 `assets/strata-queue-runtime.tar.gz`，并同步目录声明中的 SHA256。包中仅包含 `strata_queue.py`、`initial-queue.patch`、`LICENSE.txt`，文件顺序、权限、uid/gid、mtime 和 gzip 时间戳固定。

## 显式实际镜像 CPU 契约测试

`integration/` 不属于默认离线单测。其脚本显式运行时必须具备基础镜像、真实源文件及只读资产挂载，条件不满足直接失败，不会跳过。HTTP handler、frontend 和 upstream MockEngine/MCP 测试辅助来自实际镜像，没有替换为仿造 handler。

在具备足够空间、已获准使用的 Docker daemon 上，从仓库根目录执行下列命令。镜像约 6.6 GB 压缩/11.4 GB 展开；磁盘紧张时使用已经准备好的隔离 daemon，勿切换或修改共享主机 daemon。可通过 `DOCKER_HOST` 指向获准使用的 daemon。绑定挂载路径必须对该 daemon 可见。

```sh
set -eu
image='swr.cn-southwest-2.myhuaweicloud.com/gpuworks/strata@sha256:91e52d2b90644025682f5c35d45f7c13d1471c31ad32274f32af7646f37f354e'
asset_dir=$(mktemp -d)
trap 'rm -rf "$asset_dir"' EXIT
tar -xzf internal/server/inferencecatalog/seed/catalog/models/ista-daslab/qwen3.8-flash-next-strata/assets/strata-queue-runtime.tar.gz -C "$asset_dir"
docker run --rm --network=none \
  --mount "type=bind,src=$asset_dir,dst=/opt/gpuworks/strata-queue,readonly" \
  --mount "type=bind,src=$PWD/build/strata-queue/integration,dst=/gwks93-tests,readonly" \
  --entrypoint /opt/strata/.venv/bin/python \
  -e PYTHONPATH=/opt/strata "$image" /gwks93-tests/run.py
```

测试先确认实际原始源文件哈希、资产挂载 EROFS、补丁结果哈希、幂等和 argv 转交，再执行 3 项真实 HTTP wire 测试及 4 项边界测试，包含断线取消、加载失败、顺序请求清理和首段内容已可见后的 MCP 排队排除。入口 argv 在此镜像测试中用 exec mock 验证；离线单测另有真实 exec 子进程参数验证。没有调用真实 GPU 模型或原模型入口。

测试只修改该临时容器的可写层。可在退出后用新容器确认基础镜像未变：

```sh
docker run --rm --network=none --entrypoint /opt/strata/.venv/bin/python "$image" -c 'import hashlib; from pathlib import Path; assert hashlib.sha256(Path("/opt/strata/serve/server.py").read_bytes()).hexdigest()=="fc85cfd3a5b0379c8482de7a81673cae76299751aa5fe614413bc01f1fa54408"'
```

这些 CPU 检查不能代替授权的真实 GPU 模型运行、原始故障请求重现或部署验收。更新目录模板也不会重写现有部署的不可变快照；新建/重建部署才使用新的资产与启动方案。
