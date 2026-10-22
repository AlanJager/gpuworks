# ADR 0037: 试验场 HTML 预览在不透明源沙箱里运行，网络默认禁止

日期：2026-10-08。状态：已接受。

试验场对话里语言为 `html` / `htm` / `xhtml` / `svg` 的代码块，以及整条就是 HTML 文档的回复，可以在右侧抽屉里预览。预览的内容是模型输出，可能由提示注入、检索到的文档或上游模型本身决定，所以按不可信代码对待：

- **隔离**：iframe 用 `srcdoc` 加 `sandbox="allow-scripts allow-modals allow-downloads"`，**永不加 `allow-same-origin`**。页面因此是不透明源，读不到控制台的 cookie、localStorage 与 DOM；它发往控制台的请求属于跨站，`SameSite=Lax` 的会话 cookie 不会被带上。也不加 `allow-forms`、`allow-popups`、`allow-top-navigation`。无论网络开关如何，这一层都不变。
- **网络**：默认在页面 `<head>` 最前面注入 CSP。脚本、样式、图片、字体、媒体只能内联或来自 HTTPS（引用 CDN 库的页面照常工作），`connect-src 'none'` 禁止 fetch / XHR / WebSocket / EventSource / beacon，任何 plain http 子资源都被拦截。页面自带的 CSP 只会叠加收紧。管理员可在「设置 → 安全」中打开「允许试验场 HTML 预览发起网络请求」（`store.SecurityPolicy.PlaygroundPreviewNetwork`，经 `GET /api/v1/playground/models` 的 `html_preview.allow_network` 下发），打开后不注入任何网络限制。
- **eval 与 wasm 放行**：内联脚本已经放行，再禁 `eval` 没有安全收益，只会让 Vue 浏览器内编译、Alpine.js 与 wasm 页面坏掉。
- **引导脚本**：先于页面脚本执行，给 `localStorage` / `sessionStorage` / `document.cookie` 换上内存实现（不透明源访问它们会抛 `SecurityError`）；把 http(s) 链接点击交给父页面，默认策略下父页面弹确认框显示完整 URL 后才以 `noopener,noreferrer` 打开；默认策略下把 CSP 违规 `postMessage` 给父页面，预览顶栏据此说明被拦的地址，并提示需管理员开启。父页面只接受 `event.source` 为该 iframe、且形状固定的消息，内容按文本渲染。
- **渲染时机**：代码块收尾（或回复结束）后只渲染一次；生成中显示源码，避免每个 chunk 重跑页面脚本。
- **不提供「新窗口打开」**：`blob:` URL 继承控制台的源，新标签页里的脚本能带着用户的会话调管理 API。「下载」给出原始文件，本地用 `file://` 打开属于用户自己的操作。

## 权衡与后果

- 策略在查看者自己的浏览器里执行。它保护的是查看者的浏览器不被模型输出利用去访问内网和控制台，而不是服务端权限；用户能改动的只有自己那一份页面。
- 默认策略防不住内容外泄：`img-src https:` 仍允许页面把内容拼进图片地址发往外部 HTTPS 站点。它防的是从查看者浏览器向内网和控制台自身发请求。
- CSP 与 sandbox 都管不到的残留：
  - 脚本可以让 iframe 自身导航（`location = ...`、`<meta http-equiv="refresh">`），从而发出一次 GET。默认策略下父页面检测到首次以后的 `load` 就重置预览并提示；子框架导航不带 `SameSite=Lax` 与默认 cookie。
  - WebRTC 的 STUN/ICE 与 DNS 预解析不受 CSP 约束。
- 内网离线集群里 CDN 不可达时，页面能渲染，只是缺少外部库；这是预期行为。
- 存储兜底只在内存里，重新载入即丢失；顶栏有说明。
- `srcdoc` 文档继承父页面的 CSP。控制台目前不设 CSP；以后若给控制台加 CSP，需要确认它不会意外放宽或误伤预览。

## 未采用方案

- 加 `allow-same-origin` 以便 localStorage 原生可用：与 `allow-scripts` 同时出现时页面拿到控制台的源，可以移除自己的 sandbox，等于把会话交给模型输出。
- 独立的预览域名（类似 artifact 沙箱域）：要求部署方多配一个域名与证书，单二进制、HTTP 直连 IP 的常见部署做不到；不透明源已提供同等的会话隔离。
- 按 http/https 区分的网络开关档位：管理员要的是「禁止」或「放开」两种明确状态，中间档位（只允许 https）仍可达 https 内网服务，徒增理解成本。
- 每个 chunk 防抖重渲染：重跑脚本、重放动画，还会执行写了一半的 `<script>`。
