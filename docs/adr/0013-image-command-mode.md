# ADR 0013: 容器命令拼装按镜像来源分「命令模式」三档

日期: 2026-08-23 · 状态: 已接受(M20/M21 验收 grill Q17/Q25)。2026-09-15:按 ADR 0020,`manual` 模式是唯一绕过参数字典的通道——命令文本是真源、估算不适用、放置输入仍来自表单;`engine` 与 `image-entry` 模式的引擎参数全部来自单一启动配置。

2026-09-19：新部署的默认命令模式及配置来源由 [ADR 0022](0022-template-owned-deployment-launch.md) 调整；存量部署保留原有语义。

## 背景

M19 把镜像 entrypoint/cmd 收窄为纯展示(引擎 builder 完整命令无条件接管,`controller.go:540-566`),避免 inspect 回填的 entrypoint 意外顶掉引擎命令。验收发现这个一刀切对社区/自定义镜像不成立:平台确保不了外来镜像的内容,`vllm/vllm-openai` 这类社区镜像的标准用法就是「镜像自带 entrypoint 是启动器、cmd 是参数」,而模型专用镜像(如 glm-4.6-sm120)可能把命令整个写死。三种镜像需要三种拼装语义,且不同镜像 entrypoint 形态不一,无法自动推断。

## 决策

镜像 RunSpec 增加 `command_mode` 字段,三选一,按 source 给默认值,用户可改:

1. `engine`(引擎接管,官方默认):builder 完整命令,忽略镜像 entrypoint——官方镜像内容我们控制,可预期;
2. `image-entry`(镜像 entry + 引擎参数,社区默认):不覆盖 entrypoint,builder 只产参数作 cmd 追加;参数从哪个 token 起拼(是否带 `serve` 子命令)由引擎 Profile 按该引擎社区镜像惯例定义;
3. `manual`(完全自定义,自定义镜像可选):引擎参数完全不注入,镜像 entrypoint+cmd 原样跑或用户手写命令。

实例级 entrypoint/cmd 覆盖与模式正交,继续保留。部署页「最终执行预览」用与下发 InstanceDesiredState 相同的组装代码如实展示三种模式的结果。

## 后果

- (+) 社区/自定义镜像开箱即用,不再被引擎命令错误接管;
- (+) 专家可为特殊镜像选 manual 并配好参数,赋能小白直接部署;
- (−) 引擎 Profile 需维护每引擎的「社区镜像参数惯例」,上游惯例变更需跟进;
- (−) image-entry 模式下引擎参数与未知 entrypoint 的兼容性无法静态保证,靠预览 + 部署后日志排障兜底。

被否方案:维持一刀切引擎接管(社区镜像语义错误)、自动探测 entrypoint 形态(形态太杂,不可靠)。
