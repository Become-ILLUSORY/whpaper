# 可选：自建反代镜像（Cloudflare Worker）

wallhaven.cc 在部分地区不可直连。三个域名 `wallhaven.cc` / `w.wallhaven.cc` / `th.wallhaven.cc` 都在 Cloudflare 上，所以一个只放行固定路径前缀的 Worker 就能当镜像用。**这是可选项**——能直连的人完全不需要它。

| 镜像路径 | 回源 | 边缘缓存 |
|---|---|---|
| `/api/*` | wallhaven.cc | 不缓存（随机结果缓存无意义），**改写 JSON 域名** |
| `/full/*` `/original/*` `/large/*` `/medium/*` `/small/*` | w.wallhaven.cc | 7 天 |
| `/lg/*` `/orig/*` `/sm/*` | th.wallhaven.cc | 7 天 |
| `/images/*` `/w/*` | wallhaven.cc | 1 天 / 1 小时 |
| `/healthz` | 本地 | 不缓存 |

JSON 里的 `path` / `thumbs` / `url` 会被改写成镜像域名，客户端只要把镜像 URL 放进 `endpoints[0]` 即可，无需其它改动。

## 部署

### A. 纯 curl（不需要 node）

```bash
export CF_API_TOKEN=xxxx                       # 需要 Workers Scripts:Edit + Zone DNS/Routes:Edit
export CF_ACCOUNT_ID=xxxx                      # 任意 CF 页面 URL 里 /accounts/<id>/
export WH_HOST=wallpaper.example.com           # 你 CF 账号下某个 zone 的主机名
export WHPAPER_TOKEN="$(head -c 16 /dev/urandom | base64 | tr -d '/+=' | head -c 20)"
./deploy.sh
```

脚本做四件事：建 proxied A 记录（占位 `192.0.2.1`）→ 上传 Worker → 绑定路由 → 打 `/healthz`。可重复执行。

### B. wrangler

```bash
npx wrangler secret put WHPAPER_TOKEN
npx wrangler deploy
# 然后在 Dashboard 给这个 Worker 添加 Custom Domain: WH_HOST
```

## 接到客户端

```bash
whpaper config -init
# 编辑 ~/.config/whpaper/config.json：
#   "endpoints": ["https://wallpaper.example.com", "https://wallhaven.cc"],
#   "token":     "<你的 WHPAPER_TOKEN>"
whpaper probe -v -save
```

## 验证

```bash
curl -s https://$WH_HOST/healthz
curl -s -H "x-wh-token: $WHPAPER_TOKEN" \
  "https://$WH_HOST/api/v1/search?sorting=random&purity=100&categories=010&resolutions=1920x1080" | head -c 400
```

不带 token 访问 `/api/` 应返回 403；`/full/...` 第二次请求应带 `x-wh-cache: HIT`。

## 不想自建镜像？用优选 IP 直连

如果你有一批能直连 Cloudflare 的 IP（比如自己的 `best.<你的域名>` 解析到这些 IP），不必反代：在配置里设

```json
{ "best_cf_domain": "best.example.com" }
```

whpaper 会解析该域名的 A/AAAA 记录，用这些 IP 去拨号，同时保持真实 SNI/Host（证书照常校验）。这对 `wallhaven.cc` 和镜像域名都生效。`whpaper probe` 会逐个测这批 IP 的握手延迟。

## 踩坑记录

- **路由字段名**：Workers routes API 认的是 `script`，不是 `script_name`。用 `script_name` POST 会返回 success 但存成 `script: null`，边缘表现为 **error 522**（回源到占位 IP）。
- **JSON 斜杠转义**：wallhaven 返回的是 `https:\/\/w.wallhaven.cc\/full\/...`，改写时得同时处理转义和未转义两种形式。
- **`accept-encoding: identity`**：要改写 JSON 正文，必须让回源不压缩。
- **别缓存 `/api/`**：`sorting=random` 每次结果不同，缓存会让壁纸永远只有那几张。
