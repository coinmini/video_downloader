# res-downloader 项目概览

> 本文档供 Claude Code 在上下文清理后快速熟悉项目，也可作为开发参考。

## 一、项目简介

**名称**: res-downloader（爱享素材下载器）
**版本**: 3.1.3
**技术栈**: Go 1.24 + Wails v2（桌面框架）+ Vue 3 + Vite（前端）
**作用**: 跨平台资源下载工具，通过本地 HTTP/HTTPS 代理（`127.0.0.1:8899`）拦截浏览器流量，捕获并下载各平台的媒体资源（视频、图片、音频）。

支持平台：微信视频号、小程序、抖音、快手、小红书、QQ 音乐等。

## 二、目录结构

```
res-downloader/
├── main.go                  # Wails 应用入口
├── wails.json               # Wails 构建配置（版本、应用名）
├── go.mod / go.sum          # Go 依赖
├── core/                    # Go 后端核心
│   ├── app.go               # 应用生命周期、证书管理、系统代理开关
│   ├── http.go              # HTTP API 服务器（REST 接口）
│   ├── middleware.go         # 路由分发（/api/* → API 处理，其余 → 代理）
│   ├── proxy.go             # goproxy 代理服务器 + 插件注册
│   ├── config.go            # 配置管理（端口、保存目录、MIME 类型等）
│   ├── resource.go          # 资源管理（媒体列表、去重 mediaMark、下载队列）
│   ├── downloader.go        # 多线程文件下载器（重试、进度追踪）
│   ├── storage.go           # JSON 文件持久化存储
│   ├── rule.go              # URL 过滤规则
│   ├── bind.go              # Wails 前端绑定
│   ├── aes.go               # AES 解密（微信视频）
│   ├── logger.go            # zerolog 日志
│   ├── system.go            # 系统操作接口
│   ├── system_darwin.go     # macOS 系统代理/证书安装
│   ├── system_linux.go      # Linux 实现
│   ├── system_windows.go    # Windows 实现
│   ├── utils.go             # 工具函数
│   ├── shared/              # 共享类型定义
│   │   ├── base.go          # MediaInfo 结构体（资源元信息）
│   │   ├── plugin.go        # Plugin 接口 + Bridge 结构体
│   │   ├── const.go         # 常量
│   │   └── utils.go         # 通用工具（时间格式化、文件操作等）
│   ├── plugins/             # 平台插件
│   │   ├── plugin.xiaohongshu.com.go  # 小红书（最复杂，27KB）
│   │   ├── plugin.kuaishou.com.go     # 快手（GraphQL 批量抓取）
│   │   ├── plugin.qq.com.go           # QQ 音乐
│   │   └── plugin.default.go          # 通用资源检测
│   └── xhsign/             # 小红书签名算法
│       └── sign.go          # X-s / X-s-common 签名生成（从 Python 移植）
├── frontend/                # Vue 3 前端
│   ├── src/
│   │   ├── App.vue          # 根组件
│   │   ├── main.ts          # 入口
│   │   ├── views/
│   │   │   ├── index.vue    # 主页面（资源列表）
│   │   │   └── settings.vue # 设置页
│   │   ├── components/      # UI 组件
│   │   ├── api/             # 后端 API 调用封装
│   │   ├── stores/          # Pinia 状态管理
│   │   ├── locales/         # 国际化（中文/英文）
│   │   ├── router/          # Vue Router
│   │   └── types/           # TypeScript 类型定义
│   └── package.json         # 前端依赖（Naive UI、TailwindCSS）
├── build/                   # 构建产物 & 资源（appicon.png）
├── docs/                    # 文档站（docsify）
├── batch_download.py        # Python 批量下载脚本（从导出 TXT 读取）
├── 小红书自动浏览.sh          # AppleScript 自动滚动脚本
├── CLAUDE.md                # Claude Code 环境配置（Python/Node 路径、安全规范）
└── PROJECT.md               # 本文件
```

## 三、核心架构

### 3.1 工作流程

```
用户启动应用
  → Wails 初始化（main.go → core.GetApp → app.Startup）
  → 启动 HTTP 服务器（127.0.0.1:8899）
  → 启动 goproxy 代理（HTTPS MITM，需安装自签名证书）
  → 设置系统代理（可选）
  → 浏览器流量经代理拦截
  → 插件系统匹配域名，分析请求/响应
  → 提取资源信息 → EventsEmit 推送到前端
  → 用户在界面选择并下载资源
```

### 3.2 插件系统

定义在 `core/shared/plugin.go`：

```go
type Plugin interface {
    SetBridge(*Bridge)
    Domains() []string
    OnRequest(*http.Request, *goproxy.ProxyCtx) (*http.Request, *http.Response)
    OnResponse(*http.Response, *goproxy.ProxyCtx) *http.Response
}
```

`Bridge` 结构体解耦插件与核心逻辑，提供以下回调：
- `Send(type, data)` — 向前端发送事件
- `MediaIsMarked(key)` / `MarkMedia(key)` — 资源去重
- `TypeSuffix(mime)` — MIME 类型映射
- `GetResType(key)` — 资源类型过滤
- `GetConfig(key)` — 读取配置

插件在 `core/proxy.go` 的 `init()` 中注册到 `pluginRegistry` map（域名 → 插件实例）。

### 3.3 全局单例

定义在 `core/app.go`：

| 变量 | 类型 | 说明 |
|------|------|------|
| `appOnce` | `*App` | 应用实例（版本、证书、代理状态） |
| `globalConfig` | `*Config` | 配置（端口、保存目录、MIME 映射等） |
| `globalLogger` | `*Logger` | 日志 |
| `resourceOnce` | `*Resource` | 资源管理（mediaMark 去重、tasks 下载任务） |
| `systemOnce` | `*SystemSetup` | 系统操作（代理、证书） |
| `proxyOnce` | `*Proxy` | 代理服务器 |
| `httpServerOnce` | `*HttpServer` | HTTP 服务器 |
| `ruleOnce` | `*RuleSet` | URL 规则过滤 |

另外在 `proxy.go` 中有两个直接引用的插件实例：
- `kuaishouPlugin` — 快手插件
- `xiaohongshuPlugin` — 小红书插件

## 四、HTTP API 接口

路由分发在 `core/middleware.go` 的 `HandleApi()` 中，所有 `/api/*` 路径走 API，其余走代理。

| 接口 | 方法 | 说明 |
|------|------|------|
| `/api/install` | POST | 安装 SSL 证书 |
| `/api/set-system-password` | POST | 设置系统密码（macOS 安装证书用） |
| `/api/preview` | GET | 代理预览资源（绕过 CORS） |
| `/api/proxy-open` | POST | 开启系统代理 |
| `/api/proxy-unset` | POST | 关闭系统代理 |
| `/api/open-directory` | POST | 打开文件夹选择对话框 |
| `/api/open-file` | POST | 打开文件选择对话框 |
| `/api/open-folder` | POST | 在 Finder 中打开指定路径 |
| `/api/is-proxy` | GET | 查询代理状态 |
| `/api/app-info` | GET | 获取应用信息 |
| `/api/get-config` | GET | 获取配置 |
| `/api/set-config` | POST | 更新配置 |
| `/api/set-type` | POST | 设置资源类型过滤 |
| `/api/restore-media-marks` | POST | 恢复去重标记（重启后防重复） |
| `/api/clear` | POST | 清空资源列表 |
| `/api/delete` | POST | 删除指定资源 |
| `/api/download` | POST | 下载单个资源 |
| `/api/cancel` | POST | 取消下载 |
| `/api/wx-file-decode` | POST | 微信文件解密 |
| `/api/batch-export` | POST | 导出资源列表为 TXT |
| `/api/batch-export-excel` | POST | 导出资源列表为 Excel |
| `/api/cert` | GET | 下载 CA 证书文件 |
| `/api/kuaishou-fetch-list` | POST | 开始快手批量抓取 |
| `/api/kuaishou-cancel-fetch` | POST | 取消快手抓取 |
| `/api/kuaishou-fetch-status` | GET | 快手抓取状态 |

## 五、平台插件详解

### 5.1 小红书 (`plugin.xiaohongshu.com.go`)

**最复杂的插件**（27KB），核心流程：

1. **被动拦截**: 用户浏览小红书个人主页 → 代理拦截 `user_posted` API 响应
2. **提取笔记信息**: `note_id`、`type`（图文/视频）、`cover`、`xsec_token`
3. **图片笔记**: 直接提取 CDN 图片 URL → 发送前端
4. **视频笔记**: 启动 goroutine → 用签名算法调 `feed` API → 解析 `media.stream` 获取视频 URL

**签名算法** (`core/xhsign/sign.go`):
- 从 Python 项目 [xhshow](https://github.com/Cloxl/xhshow) 移植
- 生成 `X-s`（MD5 + 自定义 Base64）、`X-s-common`（RC4 加密浏览器指纹）、`X-t`（时间戳）

**限流策略**:
- 串行请求（`fetchSem` 容量 = 1）
- 基础延迟 5s + 随机抖动 0-3s
- 每 100 条视频暂停 1 小时
- 每 900 条视频暂停 3 小时
- HTTP 461 错误时暂停 1 小时
- 失败视频进入重试队列

**取消机制**:
- `stopChan` 通道用于关闭抓取即停
- `restoreMediaMarks` 接口恢复去重标记（重启后不重复抓取）

### 5.2 快手 (`plugin.kuaishou.com.go`)

1. 浏览快手页面 → 代理自动捕获 Cookie
2. 用户输入快手主页 URL → 提取 `userId`
3. 调用 GraphQL API (`visionProfilePhotoList`) 分页获取视频列表
4. 通过 `batchFetchProgress` 事件报告进度
5. 支持取消操作

### 5.3 QQ 音乐 (`plugin.qq.com.go`)

- 检测 QQ 音乐域名的媒体资源
- 基于 MIME 类型过滤

### 5.4 默认插件 (`plugin.default.go`)

- 匹配所有未被其他插件处理的域名
- 通用 MIME 类型检测

## 六、前端架构

| 技术 | 版本 |
|------|------|
| Vue | 3.2 |
| TypeScript | 4.6 |
| Vite | 3 |
| Naive UI | 2.38 |
| Pinia | 2.1 |
| TailwindCSS | - |

**主要页面**:
- `views/index.vue` — 资源列表主界面，实时展示抓取到的资源，支持筛选、批量下载、导出
- `views/settings.vue` — 设置页面（保存路径、代理、主题等）

**前后端通信**:
- REST API（前端 → 后端）
- Wails EventsEmit（后端 → 前端，事件名 `"event"`，JSON 格式 `{type, data}`）

## 七、构建与开发

```bash
# 开发模式（Go 后端 + Vite 前端热重载）
wails dev

# 构建生产包
wails build

# 仅前端开发
cd frontend && npm run dev

# 产物路径
build/bin/res-downloader.app  (macOS)
```

## 八、关键依赖

| 依赖 | 用途 |
|------|------|
| `github.com/wailsapp/wails/v2` | 桌面应用框架 |
| `github.com/elazarl/goproxy` | HTTP/HTTPS 代理 |
| `github.com/rs/zerolog` | 结构化日志 |
| `github.com/xuri/excelize/v2` | Excel 导出 |
| `github.com/matoous/go-nanoid/v2` | 唯一 ID 生成 |
| `golang.org/x/net` | 网络工具 |

## 九、辅助工具

| 文件 | 说明 |
|------|------|
| `batch_download.py` | 从导出的 TXT 文件批量下载视频（用 wget） |
| `小红书自动浏览.sh` | AppleScript 自动滚动小红书主页（模拟人类浏览，随机间隔/距离/回看） |

## 十、近期开发重点

当前分支：`xiaohongshu`

最近 10+ 次提交集中在小红书功能完善：
- 签名算法移植（xhsign）
- 分级限流策略（防 HTTP 461）
- 重试队列（失败自动重试）
- 关闭即停机制（`stopChan`）
- 重启去重（`restoreMediaMarks`）

## 十一、数据流概览

```
                    ┌──────────────┐
                    │   浏览器      │
                    └──────┬───────┘
                           │ HTTP/HTTPS 流量
                    ┌──────▼───────┐
                    │  goproxy     │
                    │  代理服务器   │
                    └──────┬───────┘
                           │ 域名匹配
                    ┌──────▼───────┐
                    │  Plugin 插件  │  ← 小红书/快手/QQ/默认
                    │  OnResponse  │
                    └──────┬───────┘
                           │ Bridge.Send()
                    ┌──────▼───────┐
                    │  HttpServer  │
                    │  EventsEmit  │
                    └──────┬───────┘
                           │ "event" → {type, data}
                    ┌──────▼───────┐
                    │  Vue 前端     │  ← 资源列表/下载/导出
                    └──────────────┘
```
