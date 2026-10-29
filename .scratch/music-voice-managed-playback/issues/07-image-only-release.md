# S-07: 完成 image-only 升级与 Songloft 发布验收

Status: ready-for-agent
Type: task
Blocked by: 02, 06, 08
External blocked by: open-xiaoai B-05/B-06，musicdl K-06（跨区发布联调）

## Body

验证 NAS image-only 从旧版升级、additive migrations、默认配置、重启后的票据失败关闭和 lease／remote 恢复。修正自升级模式与路径契约，使 image-only 明确，不把仅补 IN_DOCKER 当作完成。汇总 Songloft 服务端、插件和客户端发布证据。

### DoD

- 旧版→新版→回滚均有版本化证据；无手工 ALTER，无错误路径自替换，重启不自动抢播。
- 完整镜像／配置／DB／插件包／客户端产物对应关系明确；旧客户端保持原路径但不列最终合规。
- Go 全量与受影响客户端测试通过；跨项目必需 Songloft cases 有结果，未跑真机／手机明确 Not run。
- Swagger／API／ADR 与可观测字段同步，Apache-2.0／NOTICE、修改声明、实际 AGPL 组件源码提供义务和 FFmpeg 构建许可完成发布检查。

### Tests

- 迁移、重启、票据撤销、lease 恢复、旧客户端兼容和 image-only 回滚测试。
- 配合 runner recovery-matrix；NAS 实际升级仅在批准窗口执行并回读运行身份。

### Rollback

关闭 managed capability 并回退上一镜像；迁移保持向后兼容，不删新表或事件，记录新数据 RPO。

## Authorization

不含镜像推送、NAS 实际升级、客户端发布或真机操作；分别授权。

## Comments
