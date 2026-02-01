# 快手批量获取视频列表功能

## 概述

在 res-downloader 中添加"批量获取"功能，让用户粘贴快手主页 URL 后，自动通过 GraphQL API 分页获取该用户的所有视频，并逐条推送到现有资源列表中，然后使用已有的"批量下载"功能一键下载。

**核心思路**：利用代理已拦截到的浏览器 Cookie，从 Go 后端直接调用快手 GraphQL API (`visionProfilePhotoList`)，逐页获取视频列表，每个视频通过现有的 `newResources` 事件推送到前端。

## 修改文件清单

### 1. 新建 `core/plugins/plugin.kuaishou.com.go` — 快手插件

核心新文件，负责：
- **Cookie 捕获**：在 `OnRequest` 中拦截浏览器到 `kuaishou.com` 的请求，存储 Cookie
- **OnRequest/OnResponse 返回 nil**：让普通浏览流量继续走 DefaultPlugin 的资源检测
- **`FetchProfileVideos(userId)`**：在 goroutine 中分页调用 GraphQL API
  - 每页请求间隔 3 秒防止限流
  - 通过 `bridge.Send("newResources", mediaInfo)` 逐条推送视频
  - 通过 `bridge.Send("batchFetchProgress", ...)` 推送进度
  - 支持 `context.Cancel` 取消
- **`CancelFetch()`** / **`IsFetching()`** / **`HasCookies()`**：状态查询方法

GraphQL 请求结构：
```
POST https://www.kuaishou.com/graphql
operationName: visionProfilePhotoList
variables: { userId, pcursor, page: "profile" }
```

每条视频提取 `photo.photoUrl`（直接 MP4 链接）、`photo.coverUrl`、`photo.caption` 等字段，构造 `shared.MediaInfo` 并发送。

### 2. 修改 `core/proxy.go` — 注册插件（~5 行）

- 在 `init()` 的 `ps` 切片中添加 `&plugins.KuaishouPlugin{}`
- 导出包级变量 `kuaishouPlugin` 供 HTTP 层调用

```go
var kuaishouPlugin = &plugins.KuaishouPlugin{}

func init() {
    ps := []shared.Plugin{
        kuaishouPlugin,           // 新增
        &plugins.QqPlugin{},
        &plugins.DefaultPlugin{},
    }
    // ... 其余不变
}
```

### 3. 修改 `core/middleware.go` — 添加 3 个路由（~6 行）

在 switch 中添加：
```go
case "/api/kuaishou-fetch-list":
    httpServerOnce.kuaishouFetchList(w, r)
case "/api/kuaishou-cancel-fetch":
    httpServerOnce.kuaishouCancelFetch(w, r)
case "/api/kuaishou-fetch-status":
    httpServerOnce.kuaishouFetchStatus(w, r)
```

### 4. 修改 `core/http.go` — 添加 3 个 handler 方法（~40 行）

- `kuaishouFetchList`：接收 `{userId}` 参数，调用 `kuaishouPlugin.FetchProfileVideos(userId)`
- `kuaishouCancelFetch`：调用 `kuaishouPlugin.CancelFetch()`
- `kuaishouFetchStatus`：返回 `{isFetching, hasCookies}`

### 5. 修改 `frontend/src/api/app.ts` — 添加 3 个 API 方法（~20 行）

```typescript
kuaishouFetchList(data: { userId: string })
kuaishouCancelFetch()
kuaishouFetchStatus()
```

### 6. 修改 `frontend/src/views/index.vue` — 添加 UI（~80 行）

- **工具栏**：在更多操作的 Popover 菜单中新增"批量获取"按钮
- **Modal 弹窗**：包含 URL 输入框、开始/取消按钮、Cookie 状态提示、进度信息
- **Script 逻辑**：
  - URL 解析提取 userId
  - 调用 API 开始/取消获取
  - 监听 `batchFetchProgress` 事件显示进度
- 获取到的视频自动出现在现有资源表格中（通过已有的 `newResources` 事件处理）

### 7. 修改 `frontend/src/locales/zh.json` 和 `en.json` — 添加翻译（~8 行每文件）

新增 key：`batch_fetch`、`batch_fetch_title`、`kuaishou_url_placeholder`、`kuaishou_invalid_url`、`kuaishou_no_cookies`、`start_fetch`、`cancel_fetch`、`fetching`

## 用户操作流程

1. 开启代理拦截
2. 在浏览器中打开快手主页 `https://www.kuaishou.com/profile/xxx`（此时 Cookie 被自动捕获）
3. 点击工具栏"更多操作" → "批量获取"
4. 粘贴主页 URL，点击"开始获取"
5. 视频逐条出现在资源列表中，进度实时显示
6. 获取完成后，全选 → 批量下载

## API 验证结果（2026-02-01）

- `POST https://www.kuaishou.com/graphql` 端点在线，HTTP 200
- 无 Cookie 时返回 `result: 2`（未授权）— 预期行为
- 需要真实登录 Cookie 才能获取数据
- `visionProfilePhotoList` 操作名仍被接受（无 schema error）

## 验证方式

1. `wails dev` 启动开发模式
2. 开启代理，浏览器访问快手主页
3. 点击"批量获取"，粘贴 URL，确认 Cookie 状态为已检测
4. 点击开始，观察视频逐条出现在列表中
5. 测试取消功能
6. 全选后使用"批量下载"下载
