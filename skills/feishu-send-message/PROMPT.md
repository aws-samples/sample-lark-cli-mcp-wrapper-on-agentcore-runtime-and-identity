# 提示词：让 agent 发一条飞书测试消息

给不支持装 skill 的 agent 用（比如直接在 AWS DevOps Agent 的聊天窗口里操作）。
整段复制粘贴即可。装了 skill 就不需要这个，见同目录的 [`SKILL.md`](SKILL.md)。

## 完整流程（首次授权 + 发消息）

```
帮我用飞书给自己发一条测试消息。授权流程按下面来：

1. 先用 search_user_tools 加载工具 —— 模糊搜索（关键词）找不到这些工具，必须直接指定
   工具名称列表 ["lark-cli-mcp___lark_auth_start", "lark-cli-mcp___lark_auth_complete",
   "lark-cli-mcp___lark_exec"]，这样才能把工具的完整规格加载进来。
2. 然后用 invoke_user_tool 调用 —— 工具名要用 search_user_tools 返回的那个名字
   （lark-cli-mcp_lark-cli-mcp___lark_auth_start，注意前面多了一层前缀），
   并带上 account_id 参数。
3. 调用 lark-cli-mcp___lark_auth_start，把返回的完整 authorization_url 原样贴给我，
   不要截断、不要改写。
4. 停下等我。我会在浏览器完成飞书登录，然后回复"已登录"。
5. 收到"已登录"后，先直接调用 lark-cli-mcp___lark_exec 试一下，command 填：
   im +chat-list --as user --page-size 3
   如果成功就说明授权已自动完成，不需要再调 lark_auth_complete。
6. 如果第 5 步报还需要授权，才调用 lark-cli-mcp___lark_auth_complete，
   session_id 用第 3 步返回的那个值，不要问我要。
7. 授权通过后，用 lark_exec 执行 whoami 拿到我的 open_id（在 onBehalfOf.openId 里），
   再发消息：im +messages-send --as user --user-id <ou_xxx> --text "测试消息"
8. 把第 5、7 步的原始返回都贴给我。
```

## 只验证授权是否还有效

```
调用 lark-cli-mcp___lark_exec，command 填：im +chat-list --as user --page-size 3
把原始返回贴给我。
```

`exit_code: 0` 就是授权仍然有效。报 `authorization required` 就走上面的完整流程。

## 说明

- **第 5 步能否直接成功**取决于有没有部署 session-binding endpoint（见
  [`deploy/session-binding/`](../../deploy/session-binding/README.md)）。部署了浏览器登录后
  绑定就自动完成；没部署则必须走第 6 步。
- **第 3 步的 URL 可能被打码**成 `<redacted>`：有些传输层会过滤 URL 形状的字符串。这时让
  agent 给出 `authorization_url_without_scheme`，自己补上 `https://`。
- **有效期**：授权码约 5 分钟，所以第 4→6 步要连着做。默认配置下 token 有效期 2 小时，
  过期要重新走一遍；配了 `AGENTCORE_EXTRA_SCOPES=offline_access` 后可以自动续期。
- **`account_id`** 填部署 AgentCore Runtime 的那个 AWS 账号 ID。
