# Agg-API-DSM

一个群晖 DSM 套件，同时提供**两个上游**的能力。

下游只认一个地址、一个 API Key —— 由**模型名**决定走哪条链路。

| 前缀 | 上游 | 说明 |
|---|---|---|
| `agnes-` | Agnes AI | 多账号池化 + RPM 严格节拍限流 |
| `copilot-` | M365 Copilot | WebSocket 长连接 + 图片生成 |

**状态：开发中，尚不可安装。** 当前进度见 [docs/architecture.md](docs/architecture.md)。

---

## 为什么不是「把两个项目拼起来」

两个上游的差异不在语言（都是 Go），而在**关注点纠缠方式**：

| | agnes-hub-go | M365-Copilot2API |
|---|---|---|
| 核心难点 | 多账号池化、RPM 限流、二维校准 | WebSocket 长连接、图片配额 |
| 依赖 | 零第三方 | 7 个第三方 |

直接拼在一起只会得到两个互不相干的进程，共享不了任何东西 —— 那不叫合并，叫打包。

真正的合并点是：**「账号池 + 限流」不是 Agnes 的私产，是通用能力。**
Copilot 同样有配额、同样会 429、同样需要冷却轮换。把它从 Agnes 的业务里
**上提**到网关层共用，两个上游都受益 —— 这就是「互补而非重叠」的落点。

架构细节见 [docs/architecture.md](docs/architecture.md)。

---

## 环境要求（规划）

| 项 | 要求 |
|---|---|
| DSM 版本 | 7.0 及以上 |
| CPU 架构 | x86-64（`arch` 声明具体平台代号，覆盖 apollolake / geminilake / v1000 等） |
| 端口 | 4444（固定） |

---

## 构建

需要 Go 1.22+。

```bash
# 拉依赖（国内网络需要镜像）
export GOPROXY=https://goproxy.cn,direct
go mod tidy

# 编译
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" \
  -o dist/agg-api ./cmd/agg-api
```

打包 SPK 需要 Linux 或 Git Bash（GNU tar 才能正确保留权限位）：

```bash
./spk/build-spk.sh
```

---

## 来源与许可

本项目**从零设计**，但移植了两个既有项目的代码，特此声明来源：

| 来源 | 移植内容 | 许可 |
|---|---|---|
| [M365-Copilot2API-DSM](https://github.com/kosje/M365-Copilot2API-DSM) | Copilot 上游协议适配（WebSocket、图片生成） | AGPL-3.0 + 附加条款 |
| [agnes-hub-go](https://github.com/my788525/agnes-hub-go) | Agnes 上游协议适配、限流与池化思路 | 无 LICENSE（上游保留所有权利） |

**许可证：AGPL-3.0 + 非商业 API 中继限制附加条款**，全文见 [LICENSE](LICENSE)。

附加条款要点：

- **禁止**把本软件（或其修改版）作为付费/商业 API 中继服务运营
- 允许个人使用、非商业使用、组织内部使用，以及按 AGPL-3.0 再分发
- 商业 API 中继需另行取得商业授权

> **注意**：`agnes-hub-go` 仓库未声明 LICENSE，默认保留所有权利。
> 若你计划公开分发包含其移植代码的产物，建议先取得上游作者授权。
> 详见 [docs/architecture.md](docs/architecture.md) 的「许可证」一节。
