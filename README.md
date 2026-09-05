# whpaper

给 [Noctalia](https://github.com/noctalia-dev/noctalia) 桌面随机换 **wallhaven** 壁纸的小工具。
单个二进制、零依赖：随机抓一张 → 下载 → 自动切壁纸 → 清理旧的。

- 一条命令换一张，或交给 systemd 每半小时自动换
- 想搜什么类型就用什么类型（动漫 / 风景 / 赛博朋克 / 4K / 竖屏 / 某颜色…）
- 自动跟随你在 Noctalia 里设置的壁纸文件夹
- 中国大陆访问不了 wallhaven？内置「优选 IP 直连」和「Cloudflare 反代」两种解法

---

## 30 秒上手

```bash
# 1) 安装（Linux / macOS，自动识别架构）
curl -fsSL https://raw.githubusercontent.com/Become-ILLUSORY/whpaper/main/scripts/install.sh | bash

# 2) 体检：确认能找到 Noctalia、壁纸目录、网络通不通
whpaper doctor

# 3) 换一张
whpaper next
```

看到桌面壁纸变了、右下角弹个「壁纸已切换 · 动漫 · 3840 × 2160」就成了。
`~/.local/bin` 不在 PATH 的话：`export PATH="$HOME/.local/bin:$PATH"`。

---

## 我想搜特定类型的壁纸，怎么弄？

**核心就一句话：`whpaper next` 后面加几个参数。** 下面是常见需求，直接抄：

| 你想要的 | 命令 |
|---|---|
| 只要**动漫** | `whpaper next -categories 010` |
| 动漫 + 通用（不要真人） | `whpaper next -categories 110` |
| 搜关键词「赛博朋克」 | `whpaper next -q "cyberpunk"` |
| 搜「原神」 | `whpaper next -q "genshin"` |
| 搜「雨 夜景」 | `whpaper next -q "rain night"` |
| 只要 **4K** | `whpaper next -resolutions 3840x2160` |
| 1080P / 2K / 4K 都行 | `whpaper next -resolutions 1920x1080,2560x1440,3840x2160` |
| **至少** 2K（更大的也要） | `whpaper next -atleast 2560x1440` |
| 竖屏（手机/带鱼屏竖着） | `whpaper next -ratios 9x16` |
| 超宽屏 | `whpaper next -ratios 32x9` |
| 只要 16:9 | `whpaper next -ratios 16x9` |
| 主色调是**蓝**的 | `whpaper next -colors 0000ff` |
| 挑**收藏最多**的（更好看） | `whpaper next -sorting favorites` |
| 挑**最近上传**的 | `whpaper next -sorting date_added` |
| 只要安全内容（默认就是） | `whpaper next -purity 100` |

参数可以**叠加**，比如「只要动漫、4K、赛博朋克、按收藏排序」：

```bash
whpaper next -categories 010 -resolutions 3840x2160 -q "cyberpunk" -sorting favorites
```

### 三个最容易懵的开关（大白话）

**`-categories`（内容分类）** —— 三位开关，顺序固定是 `通用 / 动漫 / 真人`，`1`=要、`0`=不要：

| 值 | 含义 |
|---|---|
| `010` | 只要动漫 |
| `110` | 通用 + 动漫（不要真人） |
| `001` | 只要真人 |
| `111` | 全都要 |
| `100` | 只要通用（风景/建筑等） |

**`-purity`（尺度）** —— 三位开关，顺序 `安全 / 擦边 / 露骨`：

| 值 | 含义 |
|---|---|
| `100` | 只要安全（**默认**，推荐） |
| `110` | 安全 + 擦边 |
| `111` | 全部（含露骨，需登录 API key） |

> ⚠️ 这俩必须是**三位**（`100`、`010`）。写成 `-purity 1` 会被 wallhaven 直接拒绝（返回 500）。

**分辨率：`-resolutions` vs `-atleast` 二选一**
- `-resolutions`：**精确**匹配列表里的尺寸（逗号=多选一），保证像素正好。
- `-atleast`：**至少**这么大，更大的也要（结果池更大）。
- 两个都填时 `-resolutions` 优先。想「越大越好」用 `-atleast`，想「正好 4K」用 `-resolutions`。

### 想每次都生效？写进配置文件

老敲参数烦？把口味写进 `~/.config/whpaper/config.json` 的 `search` 段，以后直接 `whpaper next` 就行：

```json
{
  "search": {
    "sorting": "random",
    "purity": "100",
    "categories": "010",
    "resolutions": "3840x2160",
    "q": "cyberpunk",
    "ratios": "16x9"
  }
}
```

命令行参数**临时覆盖**配置文件，两者随时混用。

---

## 让它自动换

**方式一：systemd 定时（推荐）**
```bash
whpaper install -interval 30m        # 每 30 分钟自动换一张
systemctl --user list-timers | grep whpaper   # 查看
whpaper uninstall                    # 卸载定时
```

**方式二：桌面快捷键**（niri 示例，其它 WM 同理）
```ini
Bind = CTRL SHIFT, W, exec, whpaper next
```

**方式三：前台常驻**（临时跑跑）
```bash
whpaper watch -interval 15m
```

> Noctalia 自带的 `[wallpaper.automation]` 只能在**已有文件夹**里轮播；whpaper 是**持续从网上抓新图**，两者互补：whpaper 负责往目录里补新壁纸。

---

## 壁纸存哪了？

默认**自动跟随 Noctalia** 的设置（读取 `noctalia config export` + `~/.local/state/noctalia/settings.toml` 里的 `[wallpaper] directory`，浅色/深色目录也会按当前主题选）。想手动指定：

```bash
whpaper next -dir ~/Wallpapers
# 或写进 config.json： "directory": "~/Wallpapers"
```

`whpaper doctor` 会打印它实际探测到的目录，确认对不对。

---

## 中国大陆打不开 wallhaven？

wallhaven 在国内直连基本废。三种解法，任选：

### 方案 A：优选 IP 直连（最简单，推荐）
如果你有一批能直连 Cloudflare 的 IP（比如自己的 `best.example.com` 解析到这些 IP），填进配置即可，**不用搭任何服务**：

```json
{ "best_cf_domain": "best.example.com" }
```

whpaper 会用这些 IP 去连 wallhaven，同时保持正确的域名和证书校验。支持**多个域名**（合并成一个 IP 池）：

```json
{ "best_cf_domain": ["best.example.com", "best2.example.com"] }
```

- 解析时会自动走 DoH 兜底，避免本机 DNS 把几十个 IP 截断成几个。
- 启动时并行测一遍这批 IP 的延迟，**永远先连最快的**，坏 IP 自动垫底。
- `whpaper probe` 能看到每个 IP 的延迟和「池子健康度」（多少个可用/失效）。

命令行临时用：`whpaper next -best-cf best.example.com`

### 方案 B：自建 Cloudflare 反代
没有优选 IP、但有自己的域名和 Cloudflare 账号：部署一个反代 Worker（见 [`proxy/`](proxy/)），然后把域名填进 `endpoints`：

```json
{ "endpoints": ["https://wallpaper.example.com", "https://wallhaven.cc"] }
```

### 方案 C：普通代理
```bash
HTTPS_PROXY=http://127.0.0.1:7890 whpaper next
```

> 实测：优选 IP 直连 wallhaven 往往比自建反代还快（少一层 Worker 开销）。能配方案 A 就优先 A。

---

## 全部命令

| 命令 | 作用 |
|---|---|
| `whpaper next` | 抓一张、下载、切换（默认命令） |
| `whpaper prefetch` | 提前下载一张到缓存，不切换（配合离线瞬切） |
| `whpaper watch` | 前台常驻，按 `-interval` 定时换 |
| `whpaper probe` | 测各端点 / 优选 IP 的延迟，报告最快 |
| `whpaper doctor` | 体检：Noctalia、目录、网络 |
| `whpaper config` | 看生效配置；`-init` 生成默认配置文件 |
| `whpaper history` | 最近换过的壁纸 |
| `whpaper install` / `uninstall` | 装 / 卸 systemd 定时 |

## 全部参数

**常用**

| 参数 | 说明 |
|---|---|
| `-q` | 搜索关键词（标签/标题） |
| `-categories` | 分类掩码 `通用/动漫/真人`，如 `010` |
| `-purity` | 尺度掩码 `安全/擦边/露骨`，如 `100` |
| `-resolutions` | 精确分辨率，逗号多选一 |
| `-atleast` | 至少多大，如 `2560x1440` |
| `-ratios` | 宽高比，如 `16x9`、`9x16`、`32x9` |
| `-colors` | 主色调，如 `0000ff` |
| `-sorting` | `random`/`favorites`/`date_added`/`views`/`toplist` |
| `-keep` | 本地保留多少张（默认 40） |
| `-dir` | 壁纸目录 |
| `-silent` | 不弹通知 |
| `-v` | 详细日志（排查用） |

**进阶**

| 参数 | 说明 |
|---|---|
| `-connector` | 只切某个显示器（如 `DP-1`） |
| `-no-apply` | 只下载不切换 |
| `-dry-run` | 只挑不下载，看看会选到哪张 |
| `-auto-resolution` | 按当前显示器分辨率自动设 `-resolutions` |
| `-endpoint` | 指定 API 基址（镜像/直连） |
| `-best-cf` | 优选 IP 域名，逗号可多个 |
| `-config` | 指定配置文件路径 |
| `-json` | 输出 JSON（脚本用） |
| `-force` | 忽略「最近不重复」限制 |

## 配置文件

`~/.config/whpaper/config.json`（`whpaper config -init` 生成）。只有这几个字段，其余走默认值：

```json
{
  "endpoints": ["https://wallhaven.cc"],
  "api_key": "",
  "token": "",
  "best_cf_domain": "",
  "directory": "",
  "keep": 40,
  "notify": true,
  "search": {
    "sorting": "random",
    "purity": "100",
    "categories": "110",
    "ratios": "16x9",
    "resolutions": "1920x1080,2560x1440,3840x2160",
    "atleast": "",
    "colors": "",
    "q": ""
  }
}
```

| 字段 | 说明 |
|---|---|
| `endpoints` | API 基址列表，按顺序试。国内把镜像/优选放前面 |
| `api_key` | wallhaven 账号 key，**只有要 NSFW 时才需要**（[wallhaven.cc → API Interface](https://wallhaven.cc/settings/account)） |
| `token` | 自建反代的访问口令（方案 B 才有） |
| `best_cf_domain` | 优选 IP 域名，字符串或数组 |
| `directory` | 留空 = 自动跟随 Noctalia |
| `keep` | 本地保留张数 |
| `notify` | 是否弹通知 |
| `search` | 默认搜索口味，等价于上面那些参数 |

优先级：**命令行 > 环境变量 > 配置文件 > 内置默认**。

### 环境变量

适合 systemd / 脚本：`WHPAPER_ENDPOINT`、`WHPAPER_TOKEN`、`WHPAPER_API_KEY`、`WHPAPER_DIR`、`WHPAPER_KEEP`、`WHPAPER_CONNECTOR`、`WHPAPER_BEST_CF_DOMAIN`、`WHPAPER_PROXY`、`WHPAPER_NO_APPLY=1`、`WHPAPER_NOTIFY=0`、`WHPAPER_AUTO_RESOLUTION=1`、`WHPAPER_DOH`（自定义 DoH 服务器）。

---

## 开发

```bash
go build -trimpath -ldflags "-s -w" -o whpaper .   # 本机编译（纯标准库，无依赖）
gofmt -l . && go vet ./...
```

- `.github/workflows/ci.yml`：push/PR 跑 gofmt + vet + build + 冒烟
- `.github/workflows/release.yml`：打 `v*` tag → 交叉编译 linux/darwin × amd64/arm64 → GitHub Release
- `proxy/`：可选的 Cloudflare 反代 Worker（国内访问方案 B）

## 附录：wallhaven API 实测结论

| 参数 | 说明 | 坑 |
|---|---|---|
| `purity` | 3 位掩码 `SFW/Sketchy/NSFW` | **`purity=1` 会 500**，必须三位 |
| `categories` | 3 位掩码 `General/Anime/People` | 同上 |
| `resolutions` | 精确匹配，逗号=OR | 单数 `resolution` 无效 |
| `atleast` | 最小尺寸 | 与 `resolutions` 二选一 |
| `ratios` | 宽高比 | |
| `colors` | 主色 hex | |
| `q` | 关键词 | |
| `sorting` | `random`/`date_added`/`views`/`favorites`/`toplist`/`relevance` | |
| `seed` | 随机种子 | 匿名请求**不生效**，每次返回新 seed |
| 限流 | 匿名 45 次/分钟 | 响应头 `x-ratelimit-remaining` |
| 图片域名 | `path` 指向 `w.wallhaven.cc` | 与 API 不同主机，反代要一起代理 |

## 许可

MIT
