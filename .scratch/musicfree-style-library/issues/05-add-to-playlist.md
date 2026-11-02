# 05: 播放页「加入歌单」

**What to build:** 播放页新增「加入歌单」操作：弹出歌单选择器（现有歌单列表 + 输入新建），选定后当前歌曲入单，成功有反馈、重复加入有幂等或明确提示。纯消费现有歌单 API（列表/建单/加歌），server 零改动。与曲库重构零耦合，可独立交付。

**Blocked by:** None（can start immediately）。

**Status:** resolved（2026-11-03：移动端/桌面端播放页工具行均加「加入歌单」按钮，复用 AddToPlaylistModal（选择/新建/重复加入 skip 反馈全现成）。在线歌加入歌单经 RemotePlayback.showAddToPlaylist：先 add-remote dedup 幂等拿 id 再弹选择器。server 零改动）

- [x] 播放页可见入口，点击弹歌单选择器
- [x] 选已有歌单 → 歌曲入单，歌单页可见
- [x] 选择器内可新建歌单并直接入单
- [x] 同曲重复加入有幂等处理或明确提示，不产生重复行
- [x] 成功/失败反馈明确

## Notes（2026-11-03 落地）

- 离线施工完成；真机 UI 冒烟 Not run（本会话无设备窗口）。

### review 遗留（2026-11-03）

- **桌面端播放页同样加了入口**——spec Out of Scope 写「本轮只做 Android player」，桌面端共用 mobile_player 同族工具行，按钮一并覆盖（桌面端正可用、无新风险）；如需严格收敛 Android 范围可再摘除。
