# notion2api Sidecar 示例

这个示例把 `CLIProxyAPI` 和本地克隆的 `notion2api` 放进同一个 Docker Compose 网络里，适合先验证 sidecar 路线，而不是直接改成原生 provider。

## 文件说明

- `docker-compose.yml`: 同时启动 `CLIProxyAPI` 和 `notion2api`
- `config.yaml`: CLIProxyAPI 最小可用配置，内置 `openai-compatibility` provider
- `.env.example`: 需要你填写的端口、API Key 和 Notion 账号占位

## 使用步骤

1. 准备 notion2api 源码

默认路径是当前工作区里的：

```bash
/Users/xiaxin3/Documents/clip/_references/notion2api
```

如果你把它放到别处，修改 `.env` 里的 `NOTION2API_DIR`。

2. 准备示例环境

```bash
cd /Users/xiaxin3/Documents/clip/CLIProxyAPI/examples/notion2api-sidecar
cp .env.example .env
```

3. 填写 `.env`

至少要改这几项：

- `CLI_PROXY_API_KEY`
- `NOTION2API_API_KEY`
- `NOTION_ACCOUNTS`

4. 同步 `config.yaml` 里的两个占位 API Key

把 `config.yaml` 里的：

- `replace-with-your-cli-proxy-api-key`
- `replace-with-your-notion2api-api-key`

替换成和 `.env` 一致的值。

5. 启动

```bash
docker compose up -d --build
```

6. 验证

先看模型列表：

```bash
curl -s http://127.0.0.1:8317/v1/models \
  -H 'Authorization: Bearer your-cli-proxy-api-key'
```

再测会话透传：

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer your-cli-proxy-api-key' \
  -d '{
    "model": "notion/claude-sonnet4.6",
    "stream": false,
    "conversation_id": "demo-conv-1",
    "messages": [
      {"role": "user", "content": "先记住我最喜欢 Go"}
    ]
  }'
```

再次请求同一个 `conversation_id`，如果能延续上下文，说明 sidecar 路线已经跑通。

## 说明

- `search_metadata` 这类 Notion 自定义 SSE 事件，是否被客户端消费，取决于客户端是否读取原始流事件。
- Heavy 模式需要额外填写 `SILICONFLOW_API_KEY`。
- 这个示例不会改动你现有的 `CLIProxyAPI/config.yaml`。
