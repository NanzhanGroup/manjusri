# manjusri — 文殊通用库（已退役 · 历史留档）

> ## ⚠️ 本仓已不再被任何组件依赖
>
> **状态：退役（deprecated · 历史留档）—— 确认无误后可归档 / 删除。**
>
> - 标记日期：**2026-09-29**
> - 判定依据：全 `/data/code` 内
>   `grep -rn "NanzhanGroup/manjusri" --include=*.go --include=go.mod` → **0 处编译依赖**
>   （原先仅剩的 1 处 `device-gateway/server/session.go` 历史注释，同批已改写）。
> - 原因：本仓全部子包已迁出（见下表）；且本仓为**公开仓**，其中协议 / 信任模型不宜公开分发。
> - **删除前请确认**：下表「现归属」各新址均可正常构建，且各自同步脚本 `--check` 通过。

## 子包去向（迁出记录）

| 原子包 | 现归属（规范源） | 分发 / 说明 | 日期 |
|---|---|---|---|
| `wsfiles` — 节点身份 / wsauth v1 签名 / 上传 / 交付模式 / 领证信封 | 私有仓 **`ws-files`** 的 `wsfiles/` | 内联副本 → 6 网关 + ws-tools 的 `internal/wsfiles/`；脚本 `ws-files/scripts/wsfiles-client-sync.sh` | 2026-09-28 |
| `memorysvc` **客户端** | 私有仓 **`memory-service`** 的 `goclient/` | 内联副本 → 5 网关 + api-server + chat 的 `internal/memorysvc/`；脚本 `memory-service/scripts/memsvc-client-sync.sh` | 2026-09-29 |
| `memorysvc` **服务端** | 私有仓 **`memory-service`**（已 **PuXian 化**：`main.px` / `srv.px` / `mem.px` …，`pxc build main.px`） | 运行中服务（`/data/app/ws/core/memory-service/`）；本仓 Go 版服务端为遗留实现 | 2026-09-29 |
| `token_cache` — Config / EmbeddingEngine / CosineSimilarity | 私有仓 **`token-cache`** 的 `sharing/` | 内联副本 → chat 的 `internal/token_cache/`；脚本 `token-cache/scripts/tokencache-client-sync.sh` | 2026-09-29 |

## 为什么退役

1. **公开暴露**：本仓为公开仓（GitHub API 200）。`wsfiles` 含节点认证协议（`X-WS-Auth/Node/Ts/Nonce/Sig/Cert` + wsauth v1）、文件服务拓扑（`cn.dl` / `hk.dl`）、领证信任流程；`memorysvc` 含内部 Unix socket RPC 契约 —— 均不宜公开。
2. **包粒度拖累**：Go 的包粒度 = 文件粒度。`memorysvc` 包里 `server.go` 带 `_ "modernc.org/sqlite"`，于是**只需要客户端的网关**被迫把整套 SQLite 引擎编进自己的二进制（实测 **~3.5 MiB / 个**、7 个传递依赖模块，线上 4379 个 sqlite 符号却一次不用）。
3. **PuXian（普贤）化前置**：迁出后各网关 / 服务端除第三方 SDK 外**零跨仓 Go 依赖**，退化为「协议 + 自包含代码」，可逐文件 `.px` 重写。

## 本仓内容（留档）

```
memorysvc/     # 遗留 Go 版记忆服务（客户端 + 服务端）—— 已被 memory-service 取代
token_cache/   # 遗留共享类型 —— 已迁至 token-cache/sharing/
go.mod go.sum
```

> **不再维护**：请勿在本仓提交新代码；协议 / 类型改动一律去上表的**规范源**。

## 变更记录

- **2026-09-29**：`memorysvc` 客户端 → `memory-service/goclient`；`token_cache` → `token-cache/sharing`；服务端由 `memory-service` PuXian 版承接。**本仓自此无任何依赖方，进入退役状态。**
- **2026-09-28**：`wsfiles` 迁出至私有仓 `ws-files`（提交 `d4c6d2f`），本仓仅余 `memorysvc` / `token_cache`。
