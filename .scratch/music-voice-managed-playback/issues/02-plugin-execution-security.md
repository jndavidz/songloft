# S-02: 实施插件版本门与短期可撤销 scope

Status: ready-for-agent
Type: task
Blocked by: 01
External blocked by: —

## Body

在后端安装、上传、商店安装、更新、自动更新和启动／懒加载边界执行 minHostVersion／capabilities 校验。插件凭证按插件主体和最小 scope 迁移为短期可撤销形式，持久撤销／代次覆盖缓存与重启；保留旧消费方短并存窗口。

### DoD

- 不兼容插件在任何执行入口失败关闭，安装记录可按产品决策保留但不能运行。
- 过期、撤销、越 scope 和重启后的旧凭证均按新策略拒绝；不再以重启或百年签名替代撤销。
- manifest 与 HTTP 授权一致；不宣称 QuickJS 全部安全限制因此完成。

### Tests

- 覆盖安装／更新／加载全边界、撤销缓存、重启、旧凭证并存与撤旧、权限绕路。
- 运行 auth、jsplugin、jsruntime 相关 Go 测试和 runner plugin-policy；客户端只验已检出实际代码，未构建产物记 Not run。

### Rollback

停用新 scoped 消费面并吊销新票，必要时停用受影响插件；不恢复百年 token 的 DB 撤销旁路。

## Authorization

不发布插件、不轮换真实凭据、不修改 NAS；真实迁移另行批准。

## Comments
