# Spec pointer: 家庭音乐／语音托管播放（Songloft）

唯一跨项目主 spec：[open-xiaoai `.scratch/music-voice-managed-playback/spec.md`](../../../open-xiaoai/.scratch/music-voice-managed-playback/spec.md)。
架构证据：[现行架构报告](file:///D:/_work/dev/docs/projects/base/music-voice-managed-playback-architecture.md)。
验收结果（2026-10-30）：[托管播放验收报告](file:///D:/_work/repos/open-xiaoai/docs/plan/music-managed-playback-acceptance-20261030.md)。

本文件不复制需求或契约。Songloft 负责请求身份、插件执行安全、当前项媒体准备、固定表示与票据、本进程资源预算、托管 BFF／手机 handoff、听音事件、增量入库扫描和 image-only 发布；bridge 是唯一 LX06 控制面，kwqq 是唯一在线解析层。系统总预算由跨项目验收聚合，不假定 Songloft 能直接限制 WebGUI。

## 本地依赖图

- S-01：无本地阻塞。
- S-02：Blocked by S-01。
- S-03：Blocked by S-01；跨区契约依赖 K-01/K-02。
- S-04：Blocked by S-03。
- S-05：Blocked by S-04。
- S-06：Blocked by S-05；跨区联调依赖 B-05。
- S-07：Blocked by S-02、S-06、S-08；跨区发布依赖 B-05/B-06、K-06。
- S-08：Blocked by S-06；跨区入库联调依赖 K-05，负责增量扫描与补偿。
- S-09：Blocked by S-07，首版后非破坏性 GC。

每张 ticket 使用 `Status: ready-for-agent`。外部依赖只阻塞对应联调／发布，不当然阻塞本地离线施工。部署、真实客户端、真机和物理删除分别授权。
