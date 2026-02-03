# res-downloader

跨平台资源下载器 v3.1.3。通过 MITM 代理拦截浏览器流量，自动提取视频/图片/音频资源。

## 技术栈

- **后端**: Go 1.24 + goproxy (MITM代理)
- **桌面框架**: Wails v2 (Go↔WebView 双向通信)
- **前端**: Vue 3 + TypeScript + Naive UI + Pinia
- **代理监听**: `127.0.0.1:8899`（同时承载 HTTP API 和代理流量）

## 构建与运行

```bash
# 开发模式（wails 在 ~/go/bin/ 下）
PATH="$HOME/go/bin:$PATH" wails dev

# 生产构建
PATH="$HOME/go/bin:$PATH" wails build

# 仅前端开发
cd frontend && npm run dev
```

## 核心架构

### 插件系统

每个平台一个插件，实现 `shared.Plugin` 接口：

```
Plugin 接口:
  Domains() []string              — 负责的域名列表
  OnRequest(req) → (req, resp)    — 拦截请求（捕获Cookie、注入内容）
  OnResponse(resp) → resp         — 拦截响应（提取资源信息）
  SetBridge(*Bridge)              — 接收Bridge回调集合
```

插件通过 `Bridge.Send(eventType, data)` 发送数据到前端。

### 现有插件及模式

| 插件 | 文件 | 模式 | 说明 |
|------|------|------|------|
| 小红书 | `core/plugins/plugin.xiaohongshu.com.go` | 被动拦截 | 拦截 user_posted/feed API 响应，自适应限流，xhsign 签名 |
| 快手 | `core/plugins/plugin.kuaishou.com.go` | 主动API | 前端触发→后端 GraphQL 分页获取，Cookie 版本追踪 |
| QQ/微信 | `core/plugins/plugin.qq.com.go` | JS注入 | 替换JS版本号+注入提取脚本 |
| 默认 | `core/plugins/plugin.default.go` | Content-Type检测 | 兜底：按HTTP头识别媒体资源 |

### 事件通信流

```
Go plugin → bridge.Send(type, data)
         → runtime.EventsEmit(ctx, "event", jsonString)
         → 前端 EventsOn("event") [Wails binding]
         → useEventStore 按 type 分发到注册的 handler
         → Vue 组件更新 UI
```

### API 路由

在 `core/middleware.go` 中注册，由 `core/http.go` 实现处理函数。命名规则：`/api/{platform}-{action}`。

快手示例：`/api/kuaishou-fetch-list`, `/api/kuaishou-cancel-fetch`, `/api/kuaishou-fetch-status`

## 关键文件

### 后端 (core/)
- `core/shared/plugin.go` — Plugin 接口 + Bridge 结构体定义
- `core/shared/base.go` — MediaInfo 结构体（资源通用数据模型）
- `core/proxy.go` — 插件注册表 `pluginRegistry`、代理初始化、请求/响应分发
- `core/http.go` — HTTP API 处理函数、`send()` 事件推送
- `core/middleware.go` — API 路由表（所有 `/api/*` 路径）
- `core/app.go` — 应用生命周期、证书管理、系统代理开关
- `core/storage.go` — 资源存储（sync.Map）、去重（MD5）、下载管理
- `core/config.go` — 全局配置持久化
- `core/plugins/` — 各平台插件实现

### 前端 (frontend/src/)
- `views/index.vue` — 主页面：资源列表表格、批量操作、批量获取弹窗
- `api/app.ts` — 后端 API 调用封装
- `stores/event.ts` — Wails 事件监听 + handler 注册分发
- `stores/index.ts` — 全局状态（配置、代理状态等）
- `locales/zh.json`, `locales/en.json` — 国际化
- `assets/js/decrypt.js` — 媒体解密工具

## 添加新平台的标准步骤

1. **Go 插件**: 创建 `core/plugins/plugin.{domain}.go`，实现 `Plugin` 接口
2. **注册插件**: 在 `core/proxy.go` 的 `init()` 中添加到插件列表 + 声明全局变量
3. **API 路由**: 在 `core/middleware.go` 添加路由 case，在 `core/http.go` 实现处理函数
4. **前端 API**: 在 `frontend/src/api/app.ts` 添加对应方法
5. **前端 UI**: 在 `frontend/src/views/index.vue` 添加批量获取入口和进度显示
6. **国际化**: 在 `locales/zh.json` 和 `en.json` 添加翻译键

## 代码约定

- Go 插件文件名：`plugin.{顶级域名}.go`（如 `plugin.kuaishou.com.go`）
- API 响应格式：`{ code: 1, message: "ok", data: ... }`（code=1 成功，code=0 失败）
- 事件类型：`newResources`（新资源）、`batchFetchProgress`（批量获取进度）
- 前端使用 Vue 3 Composition API + `<script setup>` 语法
- 资源去重：`bridge.MediaIsMarked(shared.Md5(url))` 检查 → `bridge.MarkMedia()` 标记
