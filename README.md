# whpaper

给 [Noctalia](https://github.com/noctalia-dev/noctalia) 桌面随机抓 [wallhaven.cc](https://wallhaven.cc) 壁纸并自动切换的小工具。单个静态二进制，纯 Go 标准库，无 CGO、无运行时依赖。

- 按你给的搜索条件随机取一张，下载、切壁纸、清理旧图，一条龙
- 自动跟随 Noctalia 设置的壁纸目录（含浅色/深色分别配置）
- 中国大陆等访问不了 wallhaven 的网络：可自建 Cloudflare 反代镜像，或用 `best_cf_domain` 优选 IP 直连
- 去重、失败自动换候选、预取缓存、systemd 定时轮换
- 不转码：wallhaven 给的就是 jpg/png，Noctalia 原生支持

## 安装

从 Release 下载对应架构的包，解压出 `whpaper` 放进 `PATH`：

```bash
curl -fsSL https://raw.githubusercontent.com/Become-ILLUSORY/whpaper/main/scripts/install.sh | bash
```

脚本会识别系统架构、校验 sha256、装到 `~/.local/bin/whpaper`。

## 快速开始

```bash
whpaper doctor          # 体检：noctalia、目录、网络是否就绪
whpaper next            # 随机换一张
whpaper next -v         # 带调试日志
whpaper config -init    # 生成默认配置文件（可选，零配置也能跑）
whpaper install -interval 30m   # 装 systemd 用户定时器，每 30 分钟自动换
```

能直连 wallhaven 的话，`whpaper next` 开箱即用，不需要任何配置。

## 命令

| 命令 | 作用 |
|---|---|
| `next` | 抓一张随机壁纸、下载、切换（默认命令） |
| `prefetch` | 只把下一张下到暂存目录，不切换 |
| `watch` | 前台常驻，按 `-interval` 轮换 |
| `probe` | 测各镜像/优选 IP 的连通与延迟，`-save` 写回配置 |
| `config` | 查看生效配置；`-init` 写默认文件 |
| `history` | 最近切换记录 |
| `doctor` | 环境与网络体检 |
| `install` / `uninstall` | 安装 / 卸载 systemd 用户定时器 |

常用参数：`-dir` `-keep` `-q` `-resolutions` `-atleast` `-categories` `-purity` `-silent` `-v`。
完整列表见 `whpaper help`。

## 配置

零配置可用。需要时用 `whpaper config -init` 生成 `~/.config/whpaper/config.json`，文件里只有几个常用项：

```json
{
  "endpoints": ["https://wallhaven.cc"],
  "search": {
    "sorting": "random",
    "purity": "100",
    "categories": "110",
    "ratios": "16x9",
    "resolutions": "1920x1080,2560x1440,3840x2160"
  },
  "directory": "",
  "keep": 40,
  "notify": true,
  "best_cf_domain": ""
}
```

| 键 | 说明 |
|---|---|
| `endpoints` | 依次尝试的 API 基址。被墙就把自建镜像放第一个（见下） |
| `token` | 镜像的访问口令（自建镜像才需要，见 proxy/） |
| `api_key` | wallhaven 账号 API key，**只有要 NSFW 或更高限额才需要**，可留空 |
| `search` | 搜索参数，见下表 |
| `directory` | 留空 = 自动跟随 Noctalia；填了则固定到该目录 |
| `keep` | 本地保留多少张，多出的按时间删除 |
| `notify` | 换成功后发桌面通知 |
| `best_cf_domain` | 优选 IP 直连：见下 |

优先级：**命令行参数 > `WHPAPER_*` 环境变量 > 配置文件 > 内置默认**。

环境变量：`WHPAPER_ENDPOINT` `WHPAPER_TOKEN` `WHPAPER_API_KEY` `WHPAPER_DIR` `WHPAPER_KEEP` `WHPAPER_BEST_CF_DOMAIN` `WHPAPER_PROXY` `WHPAPER_CONNECTOR` `WHPAPER_NO_APPLY` `WHPAPER_AUTO_RESOLUTION` `WHPAPER_POST_APPLY_HOOK` `NOCTALIA_BIN`。

## 与 Noctalia 的目录联动

`directory` 留空时，whpaper 按 Noctalia 自己的优先级解析壁纸目录：

1. `noctalia config export`（合并后的完整配置，最准）
2. 回退：读 `~/.config/noctalia/*.toml` + **`~/.local/state/noctalia/settings.toml`**（GUI/IPC 改的设置落在这里，优先级最高）
3. 按当前主题模式选 `directory_light` / `directory_dark` / `directory`
4. 都没有 → XDG Pictures

值里的 `~`、`$HOME`、`$XDG_*`、`$VAR` 都会像 Noctalia 一样展开。

> Noctalia 自身也支持定时轮换（`[wallpaper.automation]`），但那是从**本地文件夹**里轮。whpaper 的价值是**持续从 wallhaven 拉新图**进这个文件夹再切换，两者可叠加。

## 中国大陆访问

wallhaven 在 CN 不可直连，两条路（可叠加）：

### 1. 自建反代镜像（推荐）

`proxy/` 下是一个 Cloudflare Worker，只放行 wallhaven 的固定路径前缀，并把 JSON 里的图片域名改写成你的镜像域名。部署见 [proxy/README.md](proxy/README.md)。部署后：

```json
{ "endpoints": ["https://wallpaper.example.com", "https://wallhaven.cc"], "token": "你的口令" }
```

### 2. 优选 IP 直连（`best_cf_domain`）

如果你有一批能直连 Cloudflare 的 IP（例如自己的 `best.example.com` 解析到这些 IP），不必反代：

```json
{ "best_cf_domain": "best.example.com" }
```

whpaper 会解析该域名的 A/AAAA 记录，用这些 IP 去拨号，同时保持真实 SNI/Host（证书照常校验）。这对 `wallhaven.cc` 和镜像域名都生效。

两个细节：

- **解析走 DoH 兜底**：域名挂了几十个 A 记录时，本机 resolver（glibc / systemd-resolved）常只回两三个。whpaper 会同时用 DoH（AliDNS / Cloudflare / Google，可用 `WHPAPER_DOH` 覆盖）拉取完整记录并与系统结果合并，保证拿到整池。
- **按延迟排序拨号**：启动时并行测一遍池内 IP 的握手延迟（缓存 10 分钟），之后**永远先拨最快的**，慢/不通的垫底兜底，避免随机踩到烂 IP。

`whpaper probe` 会逐个测这批 IP 的握手延迟并排序打印。

### 3. 普通代理

`WHPAPER_PROXY=http://127.0.0.1:7890` 或系统 `HTTPS_PROXY` 也支持（仅 http/https）。

## 搜索参数（实测结论）

| 参数 | 含义 | 注意 |
|---|---|---|
| `sorting` | `random` / `relevance` / `date_added` / `views` / `favorites` / `toplist` | |
| `order` | `asc` / `desc` | |
| `purity` | 3 位掩码 `SFW/Sketchy/NSFW` | `100`=纯SFW，`110`=含擦边。**`purity=1` 会 500**，必须三位 |
| `categories` | 3 位掩码 `General/Anime/People` | `010`=只要动漫，`110`=通用+动漫 |
| `resolutions` | 逗号分隔的**精确**分辨率 | 单数 `resolution` 无效 |
| `atleast` | 最小分辨率 | 与 `resolutions` 二选一 |
| `ratios` | 宽高比 `16x9` 等 | |
| `colors` | 主色调 hex | `42413c` 或 `random` |
| `q` | 关键词 | |
| `topRange` | `1d`/`1w`/`1M`/`1y`/`all` | 仅 `sorting=toplist*` |

其它：匿名默认只看 SFW；匿名限流 45 次/分钟（响应头 `x-ratelimit-remaining`）；图片直链在 `w.wallhaven.cc`，无防盗链、支持 Range、CF 缓存 30 天。

## 快捷键绑定

Noctalia 的 launcher / 合成器里绑一条命令即可手动换：

```
whpaper next
```

niri 示例：`bind = { mod = "Mod"; key = "W"; action = "Execute"; args = ["whpaper", "next"]; }`

## 开发

```bash
go build -trimpath -o whpaper .
go vet ./...
```

编译与发布都在 GitHub Actions 完成：推 `v*` 标签触发 `.github/workflows/release.yml`，交叉编译 linux/darwin × amd64/arm64 并建 Release。`ci.yml` 跑 gofmt/vet/build/smoke。

## 许可

MIT
