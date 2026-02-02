# 小红书资源下载操作流程

## 概述

res-downloader 支持从小红书批量获取图片和视频资源。通过本地代理被动拦截浏览器的 `user_posted` API 响应，获取笔记列表；对于视频笔记，自动调用 `feed` API（带签名）获取真实视频下载链接。

## 支持的资源类型

| 资源类型 | 说明 |
|---------|------|
| 图片 | 笔记封面图，格式包括 jpg、png、webp |
| 视频 | 视频笔记，自动选择最优编码（H.264 > H.265 > AV1），格式为 mp4 |

## 操作步骤

### 1. 启动代理

1. 打开 res-downloader
2. 点击首页左上角 **"启动代理"** 按钮
3. 根据系统提示 **允许安装证书文件** 并 **允许网络访问**
4. 确认系统代理已设置为 `127.0.0.1:8899`

### 2. 浏览小红书用户主页

1. 打开浏览器，访问 [小红书](https://www.xiaohongshu.com/)
2. 进入目标用户的个人主页，如：`https://www.xiaohongshu.com/user/profile/xxxxx`
3. **向下滑动页面**，让浏览器加载更多笔记
4. 图片和视频资源会自动出现在 res-downloader 的列表中

> **原理：** 代理拦截浏览器发出的 `user_posted` API 响应，从中解析笔记列表。图片笔记直接提取封面 CDN 链接；视频笔记自动调用 `feed` API 获取真实视频下载链接。

### 3. 查看和筛选资源

1. 获取到的资源会实时显示在主列表中
2. 每条资源包含：域名、类型（图片/视频）、描述、预览图、点赞数
3. 使用顶部类型筛选按钮（图片/视频）过滤资源
4. 使用搜索框按描述或 URL 搜索

### 4. 下载资源

**单个下载：** 右键点击资源，选择下载

**批量下载：**
1. 勾选需要下载的资源（或全选）
2. 点击 **"批量下载"** 按钮
3. 下载路径可在设置中配置
4. 下载状态会实时更新（等待 / 进行中 / 完成 / 失败）

### 5. 导出资源信息

- 支持将资源列表导出为 URL 列表或 Excel 文件
- 可用于后续批量处理或备份记录

## 技术架构

### 整体流程

```
1. 启动代理，浏览器访问小红书用户主页并向下滑动
       ↓
2. 代理拦截 user_posted API 响应（OnResponse）
   → 解析笔记列表（note_id, type, cover, title, xsec_token）
       ↓
3. 图片笔记：直接提取封面 CDN URL → emit 到前端列表
       ↓
4. 视频笔记：启动 goroutine 调用 feed API（带签名 + xsec_token）
   → 解析 media.stream → 提取真实视频 CDN URL → emit 到前端列表
       ↓
5. 同时拦截浏览器自身的 feed API 响应（用户点击笔记详情时）
   → 解析视频/图片详情 → emit 到前端列表
       ↓
6. 用户选择并下载
```

### 核心文件

| 文件 | 职责 |
|------|------|
| `core/plugins/plugin.xiaohongshu.com.go` | 小红书插件主文件，包含拦截和 API 调用逻辑 |
| `core/xhsign/sign.go` | X-s / X-s-common 签名算法（Go 移植自 xhshow） |

### 插件关键方法

| 方法 | 说明 |
|------|------|
| `OnRequest` | 捕获浏览器发送到 `edith.xiaohongshu.com` 和 `www.xiaohongshu.com` 的 Cookie |
| `OnResponse` | 拦截 `user_posted` 和 `feed` API 的响应，分发到对应解析函数 |
| `parseUserPostedNotes` | 解析 `user_posted` 响应中的笔记列表，逐条调用 `emitUserPostedNote` |
| `emitUserPostedNote` | 处理单条笔记：图片笔记直接 emit；视频笔记启动 goroutine 调用 feed API |
| `fetchAndEmitVideo` | 调用 `fetchVideoFromFeedAPI` 获取视频 URL，失败时回退到笔记网页 URL |
| `fetchVideoFromFeedAPI` | 构造带签名的 feed API 请求，解析返回的视频流 URL |
| `parseFeedNote` | 解析浏览器自身发起的 feed API 响应（被动拦截） |
| `extractVideoUrl` | 从 noteCard 的 `video.media.stream` 中提取视频 URL（优先 H.264） |

### 签名算法（xhsign）

签名算法移植自 [xhshow](https://github.com/Cloxl/xhshow)（Python），实现了以下功能：

| 签名头 | 生成方式 |
|--------|----------|
| `X-s` | MD5(content_string) → 124字节 payload → XOR 变换 → 自定义 Base64 编码 |
| `X-s-common` | 浏览器指纹 → RC4 加密 → CRC32 → 自定义 Base64 编码 |
| `X-t` | 当前时间戳（毫秒） |
| `X-b3-traceid` | 16位随机十六进制字符串 |
| `X-xray-traceid` | 时间戳 + 随机数组合的32位十六进制字符串 |

**关键参数说明：**
- `a1`：从 Cookie 中提取，用于 X-s 签名的核心参数
- `content_string`：`URI + JSON_body`（POST 请求）或 `URI?query`（GET 请求）
- 自定义 Base64 使用非标准字母表（`ZmserbBoHQ...`），不同于标准 Base64

### feed API 请求细节

**请求地址：** `POST https://edith.xiaohongshu.com/api/sns/web/v1/feed`

**请求体（JSON）：**
```json
{
    "source_note_id": "笔记ID",
    "image_formats": ["jpg", "webp", "avif"],
    "extra": {"need_body_topic": 1},
    "xsec_source": "pc_feed",
    "xsec_token": "从 user_posted 响应中获取的 token"
}
```

**必需的请求头：**
```
Content-Type: application/json;charset=UTF-8
Cookie: <从浏览器捕获的完整 Cookie>
User-Agent: <浏览器 UA>
Origin: https://www.xiaohongshu.com
Referer: https://www.xiaohongshu.com/
X-s: <签名算法生成>
X-t: <时间戳>
X-s-common: <签名算法生成>
X-b3-traceid: <随机生成>
X-xray-traceid: <随机生成>
```

**关键发现：`xsec_token` 是必须的！**

- `xsec_token` 来自 `user_posted` API 响应中每个笔记对象的 `xsec_token` 字段
- 每个笔记的 `xsec_token` 是唯一的，不能跨笔记复用
- `xsec_source` 固定为 `"pc_feed"`
- 缺少 `xsec_token` 会导致 HTTP 461 错误（`code: 300031, msg: "当前笔记暂时无法浏览"`）

**视频 URL 提取路径：**
```
response.data.items[].note_card.video.media.stream.{h264|h265|av1}[].master_url
```

### 并发控制

- 使用 `fetchSem`（channel，容量 3）限制同时发起的 feed API 请求数
- 避免触发小红书的频率限制

## 踩坑记录

### 1. 主动调用 API + 捕获签名 headers 方案（失败）

**尝试：** 在 `OnRequest` 中捕获浏览器发送的 `X-s`、`X-t`、`X-s-common` 等签名 headers，然后复用这些 headers 主动调用 `user_posted` API。

**结果：** HTTP 406 错误。小红书的签名是基于请求内容（URL + body）生成的，每个请求的签名不同，无法复用。

### 2. HTML 页面解析方案（失败）

**尝试：** 对每个视频笔记，GET 请求 `https://www.xiaohongshu.com/explore/{noteId}` 页面 HTML，解析 `window.__INITIAL_STATE__` 中的视频数据。

**结果：** 小红书是 SPA（单页应用），服务端返回的 HTML 只是 JS 外壳（~384KB），`__INITIAL_STATE__` 由客户端 JS 渲染生成，HTML 中不存在。curl 验证：
- 无 Cookie → 302 重定向到错误页
- 有 Cookie → SPA 外壳 HTML，无 `__INITIAL_STATE__`

### 3. xhshow 签名 + feed API 但缺少 xsec_token（失败）

**尝试：** 移植 xhshow 的签名算法到 Go，直接调用 feed API。POST body 只包含 `source_note_id`、`image_formats`、`extra`。

**结果：** HTTP 461 错误（`code: 300031, msg: "当前笔记暂时无法浏览"`）。签名本身是被接受的（不是 406），但缺少 `xsec_token` 导致风控拦截。

### 4. 被动拦截 + 签名调用 feed API + xsec_token（成功）

**最终方案：**
1. **被动拦截** `user_posted` API 响应（用户在浏览器中滑动主页时触发）
2. 从每条笔记中提取 `xsec_token`
3. 对视频笔记，使用 xhshow 签名算法 + `xsec_token` + 捕获的 Cookie 调用 feed API
4. 解析 feed API 响应中的视频流 URL

**成功关键：**
- `xsec_token` 必须从 `user_posted` 响应中获取，不能自行生成
- `xsec_source` 必须设为 `"pc_feed"`
- 签名算法需要 Cookie 中的 `a1` 值
- User-Agent 和 Origin/Referer 需要与浏览器一致

## 注意事项

- 需要在浏览器中**向下滑动**用户主页页面，让浏览器触发 `user_posted` API 请求
- 每次滑动加载一页笔记，视频笔记会自动在后台获取真实下载链接
- 软件会自动去重，同一资源不会重复添加
- 关闭软件前建议先 **关闭代理**，否则可能导致无法正常上网（可手动关闭系统代理）
- 提取的资源为小红书 CDN 直链，可直接下载，不依赖代理
- 同时支持浏览器 feed API 的被动拦截（用户手动点击笔记详情时也能自动获取）

## 常见问题

**Q: 滑动主页后列表中没有出现资源？**
A: 请确认代理已启动，浏览器的系统代理指向 `127.0.0.1:8899`。

**Q: 图片出现了但视频显示的是网页链接而不是视频链接？**
A: 检查日志中是否有 `no xsec_token found` 或 `feed API status 461` 的错误。如果有，说明 `xsec_token` 获取失败，请刷新浏览器页面重试。

**Q: 关闭软件后无法上网？**
A: 手动关闭系统代理设置即可恢复。

## 测试链接

https://www.xiaohongshu.com/user/profile/6100a16f0000000001003afc
