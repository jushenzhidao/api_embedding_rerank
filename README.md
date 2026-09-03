# Qwen Rerank → Jina 格式转换网关

将阿里云百炼（DashScope）的 `text-rerank` 服务包装成 Jina/Cohere 兼容的 rerank 端点，供 new-api / One API 直接接入。

## 核心机制

- **对外**：暴露 `POST /v1/rerank`，接收 Jina 格式请求（`query` / `documents` / `top_n` / `return_documents`）。
- **对内**：转换为百炼 `text-rerank` 请求（`input.query` / `input.documents` / `parameters.top_n`），调用后把 `output.results[]` 转回 Jina 的 `results[]`（含 `return_documents` 回填 `document.text`）。
- **base_url 复用**：上游地址不写死在配置里，而是通过请求头 `X-Base-Url` 传入（即 `https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com`），同一个网关实例可服务多个 Workspace。

## 请求头约定

| Header | 必填 | 说明 |
| --- | --- | --- |
| `Authorization` | 是 | `Bearer $DASHSCOPE_API_KEY` |
| `X-Base-Url` | 是 | 上游 base_url，如 `https://xxx.cn-beijing.maas.aliyuncs.com` |
| `X-Upstream-Key` | 否 | 显式指定上游 Key，优先级高于 Authorization（便于 new-api 注入自己的渠道 key） |
| `X-Instruct` | 否 | 百炼 rerank 的 instruction 参数，优先级高于请求体 `instruction` 字段 |

> `instruction` 也可直接放在请求体里（`{"instruction": "..."}`），两者同时存在时以 `X-Instruct` 头为准。`top_n` 超过文档数时自动截断为文档数；`top_n=0` 视为未指定。

## 运行

```bash
# 编译
go build -o rerank-gateway ./cmd/server

# 启动（环境变量可覆盖默认值）
SERVER_ADDR=:8080 \
UPSTREAM_TIMEOUT_MS=60000 \
MAX_CONCURRENCY=200 \
./rerank-gateway
```

## Docker 部署

```bash
# 构建镜像
docker build -t rerank-gateway .

# 运行
docker run -d --name rerank-gateway \
  -p 8080:8080 \
  -e SERVER_ADDR=:8080 \
  -e MAX_CONCURRENCY=200 \
  -e UPSTREAM_TIMEOUT_MS=60000 \
  rerank-gateway
```

宝塔面板里可直接用 Docker 管理器拉取镜像并启动，反代到内网域名即可。

## 接入 new-api

在 new-api 中新增自定义渠道：
- 类型选「自定义」或 OpenAI 兼容，base_url 填网关地址 `http://<host>:8080`。
- 请求时把真正的百炼 WorkspaceId 前缀通过 `X-Base-Url` 头透传（在渠道的模型映射/请求头配置里添加）。

## 高并发设计

1. **单例 `http.Client` + 连接池**：`MaxIdleConnsPerHost` 匹配并发上限，复用 TCP 连接，避免 TIME_WAIT 堆积。
2. **信号量限流**（`MAX_CONCURRENCY`）：向上游的并发数封顶，超出排队，且排队可被客户端断开取消。
3. **`sync.Pool` 缓冲复用**：序列化上游请求体复用 buffer，降低 GC 压力。
4. **超时分层**：上游整体超时（`UPSTREAM_TIMEOUT_MS`）+ `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout` 兜底。
5. **请求体大小限制**：10MB 上限，防止恶意大请求拖垮服务。
6. **禁止自动重定向**：`CheckRedirect` 返回 `ErrUseLastResponse`，防止 token 泄漏到非预期 host。

## 目录结构

```
cmd/server/main.go        服务入口 + 访问日志中间件
cmd/mockupstream/main.go  本地 mock 上游（端到端测试用）
internal/rerank/rerank.go 协议转换核心 + Service
internal/rerank/rerank_test.go 单元测试
.github/workflows/docker-build.yml  GitHub Actions：push 自动构建镜像
```

## CI/CD（GitHub Actions）

push 到 `main`/`master` 分支或打 `v*` tag 时，自动构建多架构镜像（linux/amd64 + linux/arm64）并推送到 GitHub Container Registry（ghcr.io），无需额外配置 Docker Hub 账号。

**标签规则**：
- 分支 push → `latest` + `sha-<短提交>`
- 打 tag `v1.2.3` → `1.2.3`、`1.2`、`1` + `sha-<短提交>`

**首次使用需开启包权限**（否则 push 会 403）：
GitHub 仓库 → Settings → Actions → General → Workflow permissions → 勾选 **Read and write permissions**。

**内网服务器拉取镜像**（宝塔 Docker）：

```bash
# 登录 GHCR（用 GitHub 用户名 + Personal Access Token，需 packages:read 权限）
echo $GITHUB_PAT | docker login ghcr.io -u <你的GitHub用户名> --password-stdin

# 拉取并运行
docker pull ghcr.io/<owner>/<repo>:latest
docker run -d --name rerank-gateway \
  -p 8080:8080 \
  ghcr.io/<owner>/<repo>:latest
```

> 若内网访问 ghcr.io 慢/不通，可配置镜像加速，或把工作流里的 `REGISTRY` 换成 `docker.io` 并登录 Docker Hub（需在仓库 Secrets 配 `DOCKERHUB_USERNAME`/`DOCKERHUB_TOKEN`）。

## 测试

```bash
go test ./...          # 单元测试
go build -o rerank-gateway ./cmd/server   # 编译
```
