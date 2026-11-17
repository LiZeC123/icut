# 视频切割工具

一个基于 Go + ffmpeg 的本地视频管理与切割 Web 工具。扫描指定目录下的视频，在浏览器里浏览故事板、逐秒预览、切割片段、下载与删除。

- 单二进制部署，前端页面内嵌，运行时仅依赖 `ffmpeg` / `ffprobe`
- 无数据库、无用户系统，面向内网 / 单机使用

---

# 第一部分：用户手册

## 快速开始

```bash
# 编译（需 Go 1.18+，因为用了 embed）
go build -o videocut .

# 运行（必须指定视频根目录）
./videocut -root /path/to/videos

# 可选参数
./videocut \
  -root /path/to/videos \   # 视频根目录（必填）
  -port 8080 \               # 监听端口，默认 8080
  -min-size 300              # 最小文件大小，单位 MiB，默认 300
```

启动后浏览器打开 `http://<host>:8080`。

> 首次启动会递归扫描目录，并为每个视频异步生成故事板（5×5 缩略图拼图）。视频较多或较大时，首次完整生成可能需要较长时间，生成结果会缓存为 `storyboard_<文件名>.jpg` 存放在视频根目录，之后启动直接命中。

## 界面概览

```
┌────────────┬────────────────────────────────┐
│  文件列表   │  当前文件路径 / 时间           │
│            ├────────────────────────────────┤
│  子目录/    │  [故事板] [预览]               │
│  文件名.mp4 ├────────────────────────────────┤
│  1.2 GB    │                                │
│            │        内容区                   │
│  ...       │                                │
│            ├────────────────────────────────┤
│            │  进度条 / 控制按钮 / 切割操作   │
└────────────┴────────────────────────────────┘
```

## 功能说明

### 文件列表（左侧栏）

- **两行展示**：第一行为相对路径（灰色小字，超长省略），第二行为完整文件名（超长自动换行，始终完整可见）
- **单击**：选中并加载该视频
- **悬停**出现两个按钮：
  - `⧉` 复制下载链接（`/download/<路径>`）到剪贴板
  - `✕` 删除文件（会同时删除原视频与其故事板缓存，需确认）

### 故事板 Tab

展示整个视频的 **5×5 缩略图拼图**，用于快速概览内容分布。

- 生成中显示提示，每 3 秒自动重试直到就绪
- 生成失败会显示"故事板生成失败"，可重启服务或检查 ffmpeg

### 预览 Tab

#### 浏览模式

- 拖动进度条 / 点击 `-5s / -1s / +1s / +5s` / 在时间输入框输入（`83` 或 `01:23:45` 或 `01:23`）后点"跳转"
- 画面为**原分辨率单帧截图**，拖动时旧画面保留直到新画面到达，无闪烁

#### 播放模式

- 点"▶ 从此处播放"进入播放：**每秒 1 帧**，进度条与实际播放同步推进
- 播放基于**精灵图缓冲**（每片 10 秒），若网络较差，播放会在时间片边界短暂停顿等待缓冲，不会"进度条动画面不动"
- 播放期间禁止跳转 / 切割，需先点"■ 停止"
- 停止后画面回落到当前秒的精确截图帧（保留最后一帧直到新帧到达，无闪烁）

#### 切割

1. 在任意位置点"设为起始点"
2. 移动到另一位置点"设为结束点"
3. 点"切割片段"

切割使用 ffmpeg 的 `-c copy` 流复制（秒级完成，无损），输出文件命名：

```
<原文件名>_clip_001<扩展名>
<原文件名>_clip_002<扩展名>
...
```

输出文件与源文件**同目录**。同一时刻只允许一个切割任务（再次提交会提示冲突）。

## 已知限制

| 限制                     | 说明                                                                         |
| ------------------------ | ---------------------------------------------------------------------------- |
| 播放是 1fps 幻灯片       | 不是连续视频播放；设计目标是"高延迟网络下也能稳定浏览"，不适合观赏动作场面   |
| 新文件不自动出现         | 扫描只在启动时执行；运行期间新增的文件、切割产物需**重启服务**才出现在列表中 |
| 切割产物不在列表中       | 同上                                                                         |
| 删除只清理原文件与故事板 | 已生成的 `_clip_*` 文件不会被自动清理                                        |
| 同名文件故事板可能串用   | 故事板缓存按 `BaseName` 命名，不同子目录下的同名视频会互相覆盖               |
| 播放期间不可切割         | 需先点停止                                                                   |

---

# 第二部分：架构与设计

面向后续维护者。目标是让你在改代码之前，先理解"为什么这么写"。

## 技术栈

| 层       | 选型                                                     |
| -------- | -------------------------------------------------------- |
| 后端     | Go 标准库（`net/http`、`os/exec`、`sync`），无第三方依赖 |
| 媒体处理 | 外部命令 `ffmpeg` / `ffprobe`                            |
| 前端     | 原生 JS + 原生 CSS，无框架、无构建                       |
| 部署     | 单二进制（`//go:embed index.html`）                      |

## 文件结构

```
.
├── main.go       # 全部后端逻辑（单文件）
└── index.html    # 全部前端逻辑（单文件，被 embed 进二进制）
```

## 数据流总览

```
浏览器 ──HTTP──> main.go handlers ──os/exec──> ffmpeg / ffprobe
  │                   │
  │                   ├─ videos / whitelist        (内存，扫描时填充)
  │                   ├─ storyboards (map)          (内存 + 磁盘缓存)
  │                   └─ 文件系统
  │
  └─ 渲染 + 本地切格 / 本地状态机
```

## 后端设计

### 启动流程（`main()`）

1. 解析参数，`filepath.Abs` 规整 `rootDir`
2. `scanVideos()`：递归扫描 → 填充 `videos` 列表和 `whitelist`
3. 启动 `storyboardWorkers = 2` 个 goroutine 消费 `storyboardQueue`
4. `loadStoryboardCache()`：对每个视频做缓存判定（见下），未命中入队
5. 注册路由并 `ListenAndServe`

> 注意 3 和 4 的顺序：worker 必须先于 `loadStoryboardCache` 启动，否则后者往队列发消息时无人消费（队列容量 10000 作为缓冲）。

### 核心数据结构

```go
type Video struct {
    RelPath, AbsPath string       // 相对/绝对路径
    Size int64
    BaseName, Ext    string       // 用于生成缓存文件名与切割产物名
}

type Storyboard struct {
    Data []byte   // 或
    Err  error    // 生成失败
}
```

### 路径安全：白名单

所有涉及文件访问的接口（`duration` / `screenshot` / `storyboard` / `sprite` / `clip` / `delete` / `download`）都**先用 `whitelist[relPath]` 查出绝对路径**，查不到即 404。`whitelist` 与 `videos` 同步维护：

- 扫描时：`whitelist[rel] = p`
- 删除时：`delete(whitelist, rel)`

**任何新接口都必须走这一步**，否则存在路径穿越风险。

### 故事板生成与缓存

- 生成：`ffmpeg -vf "fps=1/<interval>,scale=320:-2,tile=5x5"`，`interval = max(dur/26, 1)`
- 先试 `-skip_frame nokey`（只解关键帧，15 分钟超时），失败回退完整解码（30 分钟超时）
- 缓存路径：`rootDir/storyboard_<BaseName>.jpg`（沿用旧命名，未修同名冲突）

**缓存命中判定**（`loadStoryboardCache`）：

```
if cache 不存在                                 → 入队
else if cache.mtime + 1s < video.mtime          → 入队（判定过期，容差 1s）
else if 读 cache 失败                           → 入队
else                                            → 内存命中
```

**失败处理**：生成失败时 `os.Remove(cachePath)`，避免"旧图 mtime 更旧 → 下次启动永久过期重试"的循环。

### 并发控制

| 锁 / 信号               | 保护对象          | 策略                                                                          |
| ----------------------- | ----------------- | ----------------------------------------------------------------------------- |
| `videosMu` RWMutex      | `videos` 切片     | 读多写少                                                                      |
| `whitelistMu` RWMutex   | `whitelist` map   | 同上                                                                          |
| `storyboardsMu` RWMutex | `storyboards` map | 同上                                                                          |
| `screenshotMu` Mutex    | ffmpeg 截图       | **全局串行**（用户拖动进度条 debounce 后只发 1 个请求，串行不影响单用户体验） |
| `clipMu` TryLock        | 切割任务          | **全局互斥**，冲突返回 409                                                    |
| `storyboardQueue` chan  | 生成任务          | 容量 10000，2 个 worker                                                       |

**sprite 不走 `screenshotMu`**：前端通过 LRU + `inflight` 去重自然限制并发，不需要后端额外加锁。

### ffmpeg 调用约定

```go
runFFmpeg(timeout, args...) (stdout, stderr, err)
```

- 用 `context.WithTimeout` 强制超时，返回 `ffmpeg 超时` 而非泛化错误
- stdout / stderr 分别用 `bytes.Buffer` 收集
- **所有 ffmpeg 输出都走内存**（截图、sprite、故事板都是单张 JPEG，不存在流式场景）
- 日志截断到 2048 字符，避免刷屏

### API 一览

| 方法 | 路径                       | 说明                                    |
| ---- | -------------------------- | --------------------------------------- |
| GET  | `/api/videos`              | 视频列表 JSON                           |
| GET  | `/api/duration?path=`      | ffprobe 时长                            |
| GET  | `/api/storyboard?path=`    | 故事板 JPEG；未就绪返回 **202**         |
| GET  | `/api/screenshot?path=&t=` | 原分辨率单帧 JPEG                       |
| GET  | `/api/sprite?path=&t=`     | 10 秒精灵图（5×2，单帧 960 宽）         |
| POST | `/api/clip`                | JSON `{path,start,end}`，`-c copy` 切割 |
| POST | `/api/delete`              | JSON `{path}`，删除文件与故事板         |
| GET  | `/download/<relPath>`      | 原文件下载                              |
| GET  | `/`                        | 内嵌 `index.html`                       |

## 前端设计

### 状态对象

```js
state = {
  // 当前文件
  current, duration, currentT, playing, playSeq, busy,
  clipStart, clipEnd,

  // 故事板（异步，可能有 202 轮询）
  pollTimer, storyboardSeq,

  // 单帧截图（浏览 / seek）
  screenshotSeq, screenshotTimer, screenshotAbort,

  // sprite（播放）
  spriteCache, spriteCacheOrder, spriteInflight, spriteAbort,
}
```

`playSeq`、`storyboardSeq`、`screenshotSeq` 是三个**独立**的序列号，分别用于播放、故事板、截图三类异步请求的竞态丢弃。**不要混用**，否则会在边界场景下互相打断（例如停止播放后的 screenshot onload 误切模式打断新一轮播放）。

### 双模式预览（关键架构）

`#previewContainer` 内有两个子元素，通过 `.sprite-mode` class 切换显示：

| 模式        | 显示元素                        | 数据来源                 | 尺寸                                     |
| ----------- | ------------------------------- | ------------------------ | ---------------------------------------- |
| frame-mode  | `#previewFrame` (`<img>`)       | `/api/screenshot`        | 原分辨率，`object-fit: contain`          |
| sprite-mode | `#previewClip > #previewSprite` | `/api/sprite` + 本地切格 | 960 宽单帧，`background-position` 式偏移 |

**为什么分两个元素而不是一个**：两种模式的分辨率、缩放策略、定位方式完全不同。共用一个 `<img>` 会导致 `naturalWidth` 跳变、切格样式残留等 bug。

### 三条异步链路的职责

| 链路   | 触发场景                           | debounce | Abort                                  |
| ------ | ---------------------------------- | -------- | -------------------------------------- |
| 故事板 | 选中文件时                         | 无       | `storyboardSeq` 丢弃                   |
| 截图   | 拖动进度条 / ±秒 / 跳转 / 停止播放 | 200ms    | `screenshotAbort` + `screenshotSeq`    |
| sprite | 点播放 / 播放循环内 / 预取         | 无       | `spriteAbort`（切换文件时 abort 全部） |

### sprite 缓冲策略

- **时间片大小**：10 帧 = 10 秒（`SPRITE_SIZE = 10`）
- **布局**：`5×2`（`SPRITE_COLS × SPRITE_ROWS`）
- **单帧宽**：960 px（`/api/sprite` 内 `scale=960:-2`）
- **对齐规则**：`t0 = floor(t / 10) * 10`，seek 到 153s 实际拉取 150s 片、显示第 3 格
- **LRU**：最多 10 片（约 100 秒内容，内存 1~2 MB）
- **预取**：播放中超前 2 片（20 秒）
- **在途去重**：`spriteInflight` map 保证同一 `t0` 只发一个请求
- **阻塞式首次加载**：点播放前先 `ensureSprite(t0)`，LRU 命中则秒进，未命中则显示"准备播放..."等待

### 播放循环（`play()`）

```js
while (playing && seq === playSeq) {
  t0 = floor(currentT / 10) * 10
  sprite = cache.get(t0) || await ensureSprite(t0)  // 缓冲不足则等待
  showSpriteFrame(sprite, currentT - t0)
  currentT += 1
  prefetch(t0 + 10)  // 超前 2 片
  await sleep(1000)  // 分片等待，便于快速响应停止
}
```

- **分片等待 100ms** 而不是 `sleep(1000)`：让 `stopPlay()` 能在一帧内响应
- **缓冲不足时自然等待**：`await ensureSprite` 会阻塞推进，实现"进度条与播放同步但可能停顿"的语义

### 停止播放无闪烁（重要时序）

`stopPlay()` 调用 `seekTo(currentT, { keepOldFrame: true })`。在 `seekTo` 内部：

1. 检测到当前是 `sprite-mode` 且 `keepOldFrame` → **不立刻切回 frame-mode**
2. 新截图加载到 `#previewFrame`（此时它被 `display: none`）
3. `img.onload` 触发后才 `remove('sprite-mode')` → 用户看到的是"sprite 最后一帧 → 无缝替换为新帧"

同样机制适用于 sprite-mode 下的任何 seek（虽然播放期间禁止 seek，但停止后立即 seek 会走这条路径）。

### 切换文件清空（`selectFile()`）

**必须做的事**（缺一不可）：

1. `clearTimeout(pollTimer)` + `clearTimeout(screenshotTimer)`
2. `screenshotAbort.abort()` + `spriteAbort.abort()`
3. `spriteCache` 逐项 `URL.revokeObjectURL` 后清空
4. `resetPreviewAll()`：清 `previewFrame` / `previewSprite` 的 src 与样式，移除 `sprite-mode`
5. `resetStoryboard()`：清 `storyboardImg.src` 并显示占位
6. `storyboardSeq++`、`playSeq++`、`screenshotSeq++`：作废所有进行中的异步回调
7. `state.current = v` 等状态重置

漏掉任何一步都会导致**旧视频内容污染新视频视图**（比如旧故事板显示在新视频下）。

## 修改指南

### 想加一个新接口

1. 在 `main()` 里 `http.HandleFunc`
2. **必须**先查 `whitelist` 拿绝对路径
3. 需要 ffmpeg 时用 `runFFmpeg(timeout, args...)`，不要直接 `exec`
4. 前端调用时用 `encodeURIComponent(relPath)`（query 参数）或 `strictEncodePath`（path 段）

### 想改精灵图参数

需要同步改两处：
- 后端 `handleSprite` 的 `-vf fps=1,scale=960:-2,tile=5x2`
- 前端 `index.html` 顶部的 `SPRITE_SIZE / SPRITE_COLS / SPRITE_ROWS`

三处不一致会导致切格错位。

### 想改故事板参数

- 后端 `generateStoryboard` 的 `tile=5x5` 与 `interval`
- **缓存不会自动失效**（mtime 判定只看视频文件，不看参数版本）。改参数后需要手动删除所有 `storyboard_*.jpg` 或加一个"参数版本号"到 meta（参考讨论中的方案 B）

### 想改文件列表布局

- HTML 在 `renderFileList()` 动态生成，`dir` / `name` 是两行 div
- CSS 在 `.file-item .file-text / .dir / .name`，注意 `min-width: 0` 是 flex 子项换行的前提

## 已知技术债

| 项                              | 影响                            | 建议方向                                                                 |
| ------------------------------- | ------------------------------- | ------------------------------------------------------------------------ |
| 故事板缓存按 `BaseName` 命名    | 同名不同目录串用                | 加 sidecar meta 存 `relPath` 的 hash 做精确匹配                          |
| 扫描只启动时执行                | 新文件不出现                    | 加一个 `POST /api/rescan`，或在 `handleClip` 后 append 新文件到 `videos` |
| `handleDelete` 不清理 `_clip_*` | 残留文件                        | 删除时用 `filepath.Glob("<base>_clip_*<ext>")` 一并删除                  |
| 故事板缓存不感知参数变更        | 改 tile / interval 后旧图仍命中 | 引入 meta 版本号                                                         |
| 播放期间不能 seek               | 交互限制                        | 若需要，实现"seek 时中断播放 → 重新起播"                                 |
| 前端无单测                      | 竞态回归风险                    | 可引入 Playwright 覆盖"切换视频 / 停止播放 / 快速拖动"三条链路           |

## 调试技巧

- **查看 ffmpeg 日志**：所有失败都会通过 `logMsg` 打印被截断的 stderr，日志关键字：`截图失败`、`精灵图生成失败`、`切割失败`、`故事板生成失败`
- **故事板缓存不更新**：确认视频 mtime 是否真的变化（`stat`），以及 `cache.mtime + 1s >= video.mtime` 是否成立
- **sprite 切格错位**：检查 `SPRITE_SIZE / COLS / ROWS` 与后端 `-vf` 是否一致
- **播放卡顿**：Network 面板看 `/api/sprite` 响应时间，若单次 > 2s 说明后端 ffmpeg 慢（可能是大分辨率源），可考虑降低 `scale` 宽度





