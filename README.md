# manjusri — 文殊通用库

**文殊系统的基础库包**，提供 `memorysvc`（记忆服务客户端/服务端）等子包。不是独立二进制，被其他组件 import 使用。

## 编译

此项目是 library 包，不产生独立二进制。被以下项目 import：

```go
import "github.com/NanzhanGroup/manjusri"
```

## 依赖方

| 项目               | 用途                           |
|--------------------|--------------------------------|
| `chat`             | 使用 memorysvc 子包            |
| `weixin-gateway`   | 使用 memorysvc 客户端          |

## 子包说明

| 子包         | 说明                                               |
|--------------|----------------------------------------------------|
| `memorysvc`  | 记忆服务客户端 + 服务端（通过 Unix socket RPC）    |
| `token_cache`| token-cache 配置类型定义（被 chat/config 引用）     |

## 文件结构

- `memorysvc/server.go` — 记忆服务端实现（独立进程）
- `memorysvc/client.go` — 记忆服务客户端（Unix socket HTTP）
- `memorysvc/thread.go` — 线程管理（话题边界检测、自动切换）
- `memorysvc/types.go` — 数据类型定义
