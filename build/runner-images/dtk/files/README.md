# dtk/files/

Put `vllm-hcu-apply-patches` in this directory (a dependency of the Hygon `pre_start_argv`).

Source: the harbor.sourcefind.cn:5443/dcu/admin/base/custom:* image series (the registry is currently
unreachable; once you have the tar, extract the tool with `docker create` + `docker cp` into this
directory and the build bundles it automatically).

Measured: the company Harbor's `aiworker-dcu:vllm0.18.1` vanilla image does not contain the tool (it only has the DTK SDK).
