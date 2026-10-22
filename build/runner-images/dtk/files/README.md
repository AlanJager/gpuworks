# dtk/files/

`vllm-hcu-apply-patches` 应放在本目录（ Hygon `pre_start_argv` 的依赖）。

来源：harbor.sourcefind.cn:5443/dcu/admin/base/custom:* 系列镜像（当前 registry 不可达，
拿到 tar 后 `docker create` + `docker cp` 提取放入本目录即可，构建会自动内置）。

P0 实测：公司 Harbor 的 `aiworker-dcu:vllm0.18.1` vanilla 内没有该工具（只有 DTK SDK）。
