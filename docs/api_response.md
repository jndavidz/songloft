# API 响应格式规范

本项目采用 **RESTful 直返风格**，不使用 `{code, data, message}` 统一信封。

## 永久访问 Token（Home Assistant / ESPHome）

客户端设置中进入 Token 管理（Flutter：设置 → 账户 → 永久访问 Token；Lynx：设置 → 永久访问 Token），填写设备名称后创建。支持复制完整 Token 或 `Authorization: Bearer <token>` 请求头；完整凭证只在创建成功时显示一次，关闭后无法找回。复制失败时可在当前弹窗手动选择复制。

- 永久 Token 没有到期时间，无需刷新；服务重启、Docker 更新和客户端退出登录后继续有效。数据库丢失或恢复至创建前的备份后需重新创建。
- 首版具有管理员业务 API 权限，包括插件控制与资源访问；Token 管理接口仅接受正常登录的 Access Token，永久 Token、Refresh Token 和插件系统 Token 调用时返回 `403`。
- 每台设备可使用独立 Token。撤销后后续认证立即返回 `401`，已开始的请求不被强制中断；其他 Token 不受影响。
- 数据库仅保存凭证的 SHA-256 哈希；列表不返回完整凭证或哈希。最近使用时间最多每分钟更新一次，尚未使用时为 `null`。

| 接口 | 请求 / 响应 |
| --- | --- |
| `POST /api/v1/auth/personal-tokens` | `{"name":"Home Assistant"}` → `201`，返回管理信息及一次性的 `token` |
| `GET /api/v1/auth/personal-tokens?limit=20&offset=0` | `200`，`{"tokens":[...],"total":1,"limit":20,"offset":0}`，包含已撤销记录 |
| `DELETE /api/v1/auth/personal-tokens/{id}` | 无需请求体；成功或重复撤销返回 `204`，ID 不存在返回 `404` |

设备将凭证放在请求头 `Authorization: Bearer sl_pat_...`。图片、音频等不能设置请求头的场景支持 `?access_token=sl_pat_...`，与普通访问 Token 的使用方式相同。管理 ID 与完整凭证是两个独立值。

创建 / 列表响应设置 `Cache-Control: no-store`。名称去除首尾空白后须为 1–80 个字符，禁止控制字符，无效请求返回 `400`。普通登录 Token 仍按原有的 7 天 Access Token、30 天 Refresh Token 机制运行。

---

## 成功响应

### 单个实体 — 直接返回模型

```json
// GET /api/v1/songs/1
{"id":1, "title":"Sample Track", "artist":"Sample Artist", ...}
```

### 分页列表 — 集合名 + 分页元数据

```json
// GET /api/v1/songs?limit=20&offset=0
{"songs":[...], "total":100, "limit":20, "offset":0}
```

### 操作结果 — `{"message": "..."}`

```json
// DELETE /api/v1/songs/1
{"message": "歌曲已删除"}
```

用 `models.SuccessResponse` 或 `map[string]string{"message": "..."}` 均可。

---

## 错误响应

统一通过 `respondError` 返回，格式固定：

```json
{"error": "人类可读的错误信息", "detail": "可选的技术细节"}
```

- `error`：必有，面向用户的简短描述
- `detail`：可选，仅当传入 `err != nil` 时输出，包含底层错误信息

对应结构体：`models.ErrorResponse`。

**中间件层**同样必须返回 JSON 格式错误（使用局部 `respondAuthError`），**禁止** `http.Error()` 返回纯文本。

### 例外：二进制流端点

播放（`/songs/{id}/play`）、代理（`/proxy`）、静态文件等二进制流端点，错误可使用 `http.Error()` / `http.NotFound()`，因为客户端不期望 JSON body。

---

## 禁止事项

| 禁止 | 原因 |
|------|------|
| `{code, data, message}` 信封 | `code` 与 HTTP 状态码语义重复，增加客户端解析负担 |
| 自定义错误字段名 | 必须使用 `error` + `detail`，不得用 `message`、`msg`、`reason` 等替代 |
| API 端点返回纯文本错误 | 前端 `response.json()` 解析会抛异常（二进制流端点除外） |

---

## 实现要点

| 场景 | 使用方式 |
|------|----------|
| 返回数据 | `respondJSON(w, status, data)` — `data` 直接序列化为顶层 JSON |
| 返回错误 | `respondError(w, status, message, err)` — 自动构建 `{error, detail}` |
| 中间件错误 | `respondAuthError(w, status, message, err)` — 与 `respondError` 格式一致 |
| jsplugin 错误 | `writePluginUnavailable` — 同样使用 `{error, detail}` 字段，其中 `detail` 存放的是语义错误代码（如 `plugin_disabled` / `plugin_not_found` / `plugin_unavailable`）而非底层技术错误信息 |
