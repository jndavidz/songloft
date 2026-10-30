# S-04: 实现可撤销短期媒体票据与独立 lease

Status: done（域内，2026-10-30：票据/lease/续期/撤销/在途终止全落地；BFF 接线与真机 Not run 见 Comments）
Type: task
Blocked by: 03
External blocked by: —

## Body

新增专用 media ticket issuer 与 lease 持久模型。票据使用高熵 opaque token，绑定 TrackRef、representation、session、epoch、entry、target 和授权主体；不向设备发管理员或 plugin-system JWT。managed 的普通 remote、jsplugin、HLS 等路径只暴露合规票据 URL。

### DoD

- 过期、撤销、越 scope、异资源和跨 lease 请求均拒绝；GET/HEAD/Range 重试合法。
- stop／换代只撤销对应 lease 与在途流，同文件其他客户端不受影响。
- 续期延长原票据授权，URI 与 representation 不变且有绝对上限；到期在途连接行为明确。
- 票据元数据可恢复或重启后明确失效，日志与反代不泄露／可重放私密内容。

### Tests

- 覆盖票据鉴权、过期、撤销、续期、重启、双客户端 lease、在途断开和日志脱敏。
- 验证非同域／非票据／不匹配 session URL 被拒绝，合法绝对 Songloft 票据 URL 可接受；SSRF 重定向／DNS 与跨 scope 负例失败关闭。

### Rollback

吊销 managed tickets/leases 并关闭新路由；保留缓存和审计数据，普通旧媒体路径不受影响。

## Authorization

仅本地服务与离线测试；不改变公网反代、防火墙或 NAS 配置。

## Comments

### 实施（2026-10-30，域内）

songloft 1461ca7：TicketIssuer（CSPRNG opaque token 只存 SHA-256、全绑定维度、独立 lease、
换代撤销、续期绝对上限）+ MediaServer（ServeContent RFC 9110、穿越拒绝、guardReader 在途
终止：过期即断/lease 秒断/DB 撤销 1s 内、日志脱敏）+ migration 0041。测试 8 项 + 在途终止
单测。Not run：app 层路由接线（随托管入口上 app）、真机 GET/Range 行为。

### 真机 GET/Range（2026-10-30，NAS 实网）

- 设备（LX06）以票据 URL 拉媒体：`/media` 返回 **206** + `content_range=bytes 0-9467006/9467007`，
  随后尾部 128 字节 Range 探测同样 206——固定表示与 Range 语义在真机生效。
- 无票据/坏票据 `/media` → 401；撤销与在途终止仍以单测为准（未在设备侧制造撤销窗口）。
