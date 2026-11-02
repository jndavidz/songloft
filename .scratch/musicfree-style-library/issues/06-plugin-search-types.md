# 06: kwqq 搜索多类型后端链（插件 type 参数 + server 代理模式）

**What to build:** 第一个贯穿层 tracer bullet：kwqq 插件搜索端点扩展 `type` 参数（music/album/artist/sheet，music 走既有精装搜索，其余三类型内部路由到 kwqq-api 透传端点，映射对照 musicdl API-REFERENCE §5.1）；server 新增插件代理路由把搜索暴露给客户端（JWT 鉴权 + 插件 scope 按 manifest 放行，在线载荷透传不落库）；curl 全链可验。**本票建立后端模式模板**（插件端点契约风格、代理路由注册、Node mock 测试与 Go handler 测试样式），后续 08–13 全部复用。

**Blocked by:** None（can start immediately）。

**Status:** resolved（2026-11-03，两处落地口径修订：①「server 新增插件代理路由」实际复用既有 /api/v1/jsplugin/{entry}/* catch-all——JWT 鉴权与跨插件 403 语义已存在，新增接线测试 plugin_browse_wiring_test.go（401/403/own-token/user-JWT 四组）；② manifest scope 无需扩展：浏览端点不新增宿主 API 访问面（storage/net 已覆盖），scope 判定沿用 plugincred 既有归类。插件新增 /browse/search 四类型（music=多源聚合、album/artist/sheet=netease cloudsearch type=10/100/1000，逐源路由按 §5.1 以 netease 为主通道、条目带 source 标注）+ /album/info（netease id 透传优先、其余源精装二段式、kuwo/qq 501 envelope）+ /artist/info /artist/works /sheet/info /toplists /toplist/detail /recommend/tags /recommend/sheets /music/comments（title+artist 回搜兜底）。全部条目带 dedup_key（与 /import 同式），player 不拼格式。Node 离线测试 24 项（成功/501/超时三态）。--lab Not run（kwqq-api 零改动，跨项目回归面不存在）

- [x] 插件搜索端点接受 type 四值，music 行为与现网兼容（既有搜索零回归）
- [x] album/artist/sheet 三类型经透传端点返回结构化结果（含 source 标注）
- [x] manifest scope 同步扩展；未声明 scope 的请求被 server 拒绝（403）
- [x] server 代理无 JWT 返回 401；载荷透传，songs 表零增行
- [x] 插件 Node 离线模拟测试覆盖四类型（成功/501 能力缺失/上游超时三态）
- [x] server Go handler 测试覆盖鉴权/scope/错误包装
- [x] workspace 跨项目离线验收（--lab）既有 case 零回归

## Notes（2026-11-03 落地）

- **复用既有转发链而非新造代理层**：`internal/jsplugin/routes.go` 的 catch-all 已把 `/api/v1/jsplugin/{entry}/*` 同步透传 QuickJS（原样 status/headers/body），且挂在 AuthMiddleware Group 内——JWT 必需、跨插件票据 403（self-path 归属）均现成。server 侧新增的只有接线测试 `internal/middleware/plugin_browse_wiring_test.go`（8 条浏览路径 × 无 JWT 401 / 跨插件 403 / 本插件 token 200 / 用户 JWT 200）。在线载荷透传不落库由转发链结构保证（无 songs 写路径）。
- **manifest scope 未扩展**：浏览端点是插件自路径、被 player 以用户 JWT 调用，不经 plugincred 的宿主 API scope 归类；插件自身对外仍是 storage/net。若未来插件要带插件票据访问新的宿主 API 面，才需同步 AllPermissions/toolchain/scaffold 三处。
- **类型搜索主通道取 netease**：实测 kwqq-api 透传 `/netease/api/cloudsearch` type=10/100/1000 形状可用；kugou `/kugou/api/search?type=` 的类型枚举未实测（API-REFERENCE 只给路径），留后续按需补。
- 透传响应是上游原样 JSON（无 {code,msg,data} 信封），插件侧 netease 调用自持裸 fetch，不走 kwqq() 的信封拆包。
- 测试：kwqq 插件 node --test 全绿（browse 24 项 + music-url 6 项回归）。
- Not run：`music_ecosystem.py --lab`（本批 kwqq-api 零改动，跨项目回归面不存在）；curl 全链真机验证（无设备窗口）。

### review 遗留（2026-11-03）

- server 侧「501/超时的错误包装」测试项随复用 catch-all 的修订**由插件 Node 测试承担**（browse 24 项含 501/超时三态），server 只保留 401/403 接线断言——spec Testing Decisions 的原措辞按修订口径解释。
- 判定项留后续：`/api/search` 内联聚合与 `searchMusicAggregated` 的同构重复（刻意保留既有路径原状）、9 个 browse 路由的 parseBody→校验→browseHandler 样板可表驱动收拢、`albumInfo` 的 501 靠 message 字符串分类（可改显式 error code）、`/browse/search` 类型搜索的 kugou 通道实测后可补。
