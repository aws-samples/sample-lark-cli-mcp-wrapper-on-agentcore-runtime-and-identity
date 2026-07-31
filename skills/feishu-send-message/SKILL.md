---
name: feishu-send-message
description: 通过 lark-cli-mcp 工具向飞书（Feishu/Lark）用户或群组发送消息。涵盖完整的 OAuth 授权流程（lark_auth_start → 浏览器登录 → lark_exec 验证）以及消息发送命令。适用于：发送飞书通知、测试飞书集成、向指定用户或群聊发消息。Load this skill when the user asks to send a Feishu/Lark message, set up Feishu auth, or use lark-cli.
---

# 飞书消息发送 (Feishu / Lark)

> 不方便装 skill 的场景（比如直接在聊天窗口里操作），用同目录下的
> [`PROMPT.md`](PROMPT.md)，里面是可以整段粘贴的提示词。

## 工具加载（必须先做）

lark-cli 工具不在默认工具列表中，必须用 `search_user_tools` 显式加载，**模糊搜索无效**，需直接指定工具名称列表：

```python
search_user_tools(tool_names=[
    "lark-cli-mcp___lark_auth_start",
    "lark-cli-mcp___lark_auth_complete",
    "lark-cli-mcp___lark_exec"
])
```

加载后实际调用名带前缀，格式为：
- `lark-cli-mcp_lark-cli-mcp___lark_auth_start`
- `lark-cli-mcp_lark-cli-mcp___lark_auth_complete`
- `lark-cli-mcp_lark-cli-mcp___lark_exec`

所有调用都需要传 `account_id: "<aws-account-id>"` —— 换成部署 AgentCore Runtime 的那个 AWS 账号 ID。

---

## 授权流程

### 步骤 1：发起授权

```python
invoke_user_tool(
    tool_name="lark-cli-mcp_lark-cli-mcp___lark_auth_start",
    arguments={"account_id": "<aws-account-id>"}
)
```

把返回的完整 `authorization_url` 原样发给用户（不要截断），然后**停下等待**用户完成浏览器登录。

### 步骤 2：验证授权（用户回复"已登录"后）

直接调用 `lark_exec` 试一下，**不需要先调 `lark_auth_complete`**：

```python
invoke_user_tool(
    tool_name="lark-cli-mcp_lark-cli-mcp___lark_exec",
    arguments={
        "account_id": "<aws-account-id>",
        "command": "im +chat-list --as user --page-size 3"
    }
)
```

- `exit_code: 0` → 授权已自动完成，直接进入发消息步骤
- 失败 → 再调用 `lark_auth_complete`，传入 `lark_auth_start` 返回的 `session_id`

---

## 获取当前用户 open_id

```python
invoke_user_tool(
    tool_name="lark-cli-mcp_lark-cli-mcp___lark_exec",
    arguments={
        "account_id": "<aws-account-id>",
        "command": "whoami"
    }
)
```

返回结构中 `onBehalfOf.openId` 即为 `open_id`（格式：`ou_xxx`）。

---

## 发送消息

### 发给指定用户（私信）

```python
invoke_user_tool(
    tool_name="lark-cli-mcp_lark-cli-mcp___lark_exec",
    arguments={
        "account_id": "<aws-account-id>",
        "command": "im +messages-send --as user --user-id <open_id> --text \"消息内容\""
    }
)
```

⚠️ 参数注意：
- 用 `--user-id`（不是 `--receive-id`）
- 用 `--chat-id`（不是 `--user-id`）发给群组

### 发给群聊

```python
"command": "im +messages-send --as user --chat-id <chat_id> --text \"消息内容\""
```

### 获取 chat_id 列表

```python
"command": "im +chat-list --as user --page-size 10"
```

---

## 常用命令速查

| 目的 | 命令 |
|------|------|
| 查看当前用户信息 | `whoami` |
| 列出最近聊天 | `im +chat-list --as user --page-size 10` |
| 发私信给用户 | `im +messages-send --as user --user-id <ou_xxx> --text "..."` |
| 发消息到群聊 | `im +messages-send --as user --chat-id <oc_xxx> --text "..."` |
| 发 Markdown | `im +messages-send --as user --user-id <ou_xxx> --markdown "**加粗** 内容"` |
| 查看命令帮助 | `im +messages-send --help` |

---

## 注意事项

- 授权有效期有限，下次使用如遇鉴权失败，重新走授权流程即可
- `lark_auth_start` 的 `session_id` 有效期约 5 分钟，需在浏览器登录后尽快验证
- 消息内容含空格时用引号包裹
