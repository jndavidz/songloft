# 06: kwqq 搜索多类型后端链（插件 type 参数 + server 代理模式）

**What to build:** 第一个贯穿层 tracer bullet：kwqq 插件搜索端点扩展 `type` 参数（music/album/artist/sheet，music 走既有精装搜索，其余三类型内部路由到 kwqq-api 透传端点，映射对照 musicdl API-REFERENCE §5.1）；server 新增插件代理路由把搜索暴露给客户端（JWT 鉴权 + 插件 scope 按 manifest 放行，在线载荷透传不落库）；curl 全链可验。**本票建立后端模式模板**（插件端点契约风格、代理路由注册、Node mock 测试与 Go handler 测试样式），后续 08–13 全部复用。

**Blocked by:** None（can start immediately）。

**Status:** ready-for-agent

- [ ] 插件搜索端点接受 type 四值，music 行为与现网兼容（既有搜索零回归）
- [ ] album/artist/sheet 三类型经透传端点返回结构化结果（含 source 标注）
- [ ] manifest scope 同步扩展；未声明 scope 的请求被 server 拒绝（403）
- [ ] server 代理无 JWT 返回 401；载荷透传，songs 表零增行
- [ ] 插件 Node 离线模拟测试覆盖四类型（成功/501 能力缺失/上游超时三态）
- [ ] server Go handler 测试覆盖鉴权/scope/错误包装
- [ ] workspace 跨项目离线验收（--lab）既有 case 零回归
