// kwqq 音源插件 —— 包装自建 kw-qq-music-api（kwqq）
// 契约：POST /api/search、POST /api/music/url（@songloft/plugin-sdk 约定）；
//       GET /lyric-search、GET /cover-search（宿主歌词/封面提供者，onInit 注册）；
//       GET/POST /config（设置页读写，storage 持久化）；
//       POST /browse/*（player 在线浏览：多类型搜索/专辑/歌手/歌单/榜单/推荐/评论，
//       spec: songloft .scratch/musicfree-style-library，端点按 MusicFree 函数语义归一）。
// 运行环境：宿主 QuickJS（es2020，全局 fetch 免权限）。不依赖 Node 内置模块。
import {
  createRouter,
  jsonResponse,
  parseQuery,
  createSearchHandler,
} from '@songloft/plugin-sdk'

// 音质档位 hint 防御归一（spec: songloft .scratch/audio-quality-routing D1/D6）。
// 与 @songloft/plugin-sdk 的 resolveQualityHint 同表——SDK 2.19.0 才携带该导出，
// kwqq 构建走 npm 已发布版，故此处内联；归一/192k snap/per-source 白名单的
// 单点真相在 kwqq-api 的 quality.py（ADR-0003），两张表都只做"合法透传/越权归 320k"。
const QUALITY_TIERS = ['128k', '192k', '320k', 'low', 'standard', 'high', 'super']
const DEFAULT_QUALITY_TIER = '320k'
function resolveQualityHint(raw, fallback) {
  const v = (raw == null ? '' : String(raw)).trim().toLowerCase()
  if (!v) return fallback
  return QUALITY_TIERS.includes(v) ? v : DEFAULT_QUALITY_TIER
}

// 音质解析优先级：按源覆盖 > App 下发 hint > 插件「音质封顶」(cfg.quality，兼容旧配置)。
// 按源覆盖放在插件侧，是因为只有在 source_data.s 到手时才知道真实音源（App 侧歌曲
// 没有 source 字段，无法在播放时按源选档）；旧配置没有 quality_by_source 时行为不变。
function resolveSourceQuality(cfg, source, hint) {
  const overrides = (cfg && cfg.quality_by_source) || {}
  const override = overrides[source]
  if (override) return resolveQualityHint(override, DEFAULT_QUALITY_TIER)
  return resolveQualityHint(hint, (cfg && cfg.quality) || DEFAULT_QUALITY_TIER)
}

const ALL_SOURCES = ['kuwo', 'qq', 'qianqian', 'migu', 'netease', 'kugou', 'bilibili']
// 歌词回搜顺序：酷我歌词链路最稳；qianqian 的歌词 URL 只随搜索结果下发（靠门面进程内缓存），故排最后
const LYRIC_SOURCES = ['kuwo', 'qq', 'netease', 'migu', 'kugou', 'qianqian']

// 默认源 = 六个 mp3 源。bilibili 在设置页手动启用（默认关）：
//   App 内试听可用（宿主代理会带 Referer/UA 拉 fMP4/m4s），但 **DLNA 投送不可用**（音箱解不了 fMP4 分片）。
const DEFAULT_SOURCES = ['kuwo', 'qq', 'qianqian', 'migu', 'netease', 'kugou']

const DEFAULTS = {
  base: 'http://10.10.10.2:3003',
  key: '',
  sources: DEFAULT_SOURCES,
  quality: '320k',
  // 按源覆盖档位：{ kuwo: '192k', netease: 'flac' } 之类；缺省/空串 = 跟随全局
  quality_by_source: {},
}

// ===== 简易 TTL 缓存（Map + 时间戳，QuickJS 兼容） =====
function ttlCache(ttlMs, max) {
  const m = new Map()
  return {
    get(k) {
      const v = m.get(k)
      if (!v) return null
      if (Date.now() - v.t > ttlMs) { m.delete(k); return null }
      return v.v
    },
    set(k, v) {
      if (m.size >= max) { const fk = m.keys().next().value; m.delete(fk) }
      m.set(k, { t: Date.now(), v })
    },
    clear() { m.clear() },
  }
}
const searchCache = ttlCache(60_000, 200)
const lyricCache = ttlCache(24 * 3600_000, 300)
const coverCache = ttlCache(24 * 3600_000, 300)
const failCache = ttlCache(60_000, 100)
const browseCache = ttlCache(10 * 60_000, 300)   // 浏览态（专辑/歌手/歌单/榜单/推荐），在线浏览不落库、仅内存缓存
const commentCache = ttlCache(60_000, 100)        // 评论短缓存（热评随点赞变动）
const cfgCacheKey = '__cfg__'
const inflight = new Map()

// ===== 配置（storage 持久化） =====
let cfgCache = null
async function getConfig() {
  if (cfgCache) return cfgCache
  let saved = {}
  try {
    saved = (await songloft.storage.get('config')) || {}
  } catch (e) { /* storage 不可用时用默认值 */ }
  cfgCache = Object.assign({}, DEFAULTS, saved)
  if (!Array.isArray(cfgCache.sources) || !cfgCache.sources.length) cfgCache.sources = ALL_SOURCES
  if (!cfgCache.quality_by_source || typeof cfgCache.quality_by_source !== 'object') {
    cfgCache.quality_by_source = {}
  }
  return cfgCache
}

/// 按源覆盖档位只保留合法音源与合法档位；空串/非法值 = 清掉该源覆盖（跟随全局）。
function normalizeQualityBySource(raw) {
  const out = {}
  if (!raw || typeof raw !== 'object') return out
  for (const s of ALL_SOURCES) {
    const v = raw[s]
    if (typeof v !== 'string' || !v.trim()) continue
    const tier = v.trim().toLowerCase()
    if (QUALITY_TIERS.includes(tier)) out[s] = tier
  }
  return out
}

async function saveConfig(patch) {
  const cfg = Object.assign({}, await getConfig(), patch)
  if (!Array.isArray(cfg.sources) || !cfg.sources.length) cfg.sources = ALL_SOURCES
  try { await songloft.storage.set('config', cfg) } catch (e) { /* 只影响重启后丢失 */ }
  cfgCache = cfg
  searchCache.clear(); lyricCache.clear(); coverCache.clear(); failCache.clear()
  browseCache.clear(); commentCache.clear()
  return cfg
}

// ===== kwqq-api 客户端（信封拆包 + 超时） =====
// 注意：宿主 QuickJS 没有 AbortController/AbortSignal.timeout，
// 超时只能 Promise.race（输掉的 fetch 由宿主自身超时兜底回收）。
function withTimeout(promise, ms, tag) {
  return Promise.race([
    promise,
    new Promise((_, rej) => setTimeout(() => rej(new Error((tag || 'fetch') + ' timeout ' + ms + 'ms')), ms)),
  ])
}

async function kwqq(cfg, path, timeoutMs) {
  const url = cfg.base.replace(/\/+$/, '') + path
  const headers = {}
  if (cfg.key) headers['X-API-Key'] = cfg.key
  const resp = await withTimeout(fetch(url, { headers }), timeoutMs || 8000, 'kwqq')
  if (resp.status === 401) throw new Error('kwqq 401：API key 缺失或错误（设置页填 key，或让 kwqq-api 信任当前网段）')
  if (!resp.ok) throw new Error('kwqq http ' + resp.status)
  const j = await resp.json()
  // 透传端点（/kugou/api/*、/netease/api/*）是上游原样 JSON，没有 {code,msg,data} 信封：
  // 只有带数字 code 的精装响应才做信封校验与拆包，否则原样交给调用方按上游形状取字段。
  if (j && typeof j.code === 'number') {
    if (j.code !== 200) throw new Error('kwqq ' + j.code + ': ' + (j.msg || ''))
    return j.data
  }
  return j
}

// ===== 结果映射 =====
function itemToResult(it) {
  const sd = {
    s: it.source,
    i: String(it.id),
    n: it.name || '',
    a: it.singer || '',
    al: it.album || '',
    d: Number(it.duration_s) || 0,
  }
  const c = it.extra && it.extra.copyrightId
  if (c) sd.c = String(c)
  return {
    title: it.name || 'Unknown',
    artist: it.singer || 'Unknown',
    album: it.album || undefined,
    duration: Number(it.duration_s) || 0,
    cover_url: it.cover || undefined,
    source_data: sd,
  }
}

// 按 标题+歌手 简单评分挑最佳匹配（宿主回搜用：无 source_data 只有元数据）
function pickBest(items, title, artist) {
  if (!items || !items.length) return null
  const T = String(title || '').toLowerCase().replace(/\s+/g, '')
  const A = String(artist || '').toLowerCase().replace(/\s+/g, '')
  let best = null, bestScore = -1
  for (const it of items) {
    const n = String(it.name || '').toLowerCase().replace(/\s+/g, '')
    const s = String(it.singer || '').toLowerCase().replace(/\s+/g, '')
    let score = 0
    if (n === T) score += 4; else if (T && n.indexOf(T) >= 0) score += 2
    if (A && s.indexOf(A) >= 0) score += 2
    if (score > bestScore) { bestScore = score; best = it }
  }
  return bestScore > 2 ? best : (items[0] || null)
}

// 回搜（标题+歌手 → kwqq item），带缓存与 in-flight 合并
async function findHit(title, artist) {
  const key = 'hit|' + title + '|' + artist
  const cached = searchCache.get(key)
  if (cached) return cached
  if (failCache.get(key)) return null
  const p = inflight.get(key)
  if (p) return p
  const task = (async () => {
    const cfg = await getConfig()
    for (const s of cfg.sources) {
      try {
        const data = await kwqq(cfg, '/' + s + '/search?keywords=' +
          encodeURIComponent((title + ' ' + artist).trim()) + '&limit=5', 5000)
        const hit = pickBest(data.items || [], title, artist)
        if (hit) {
          const item = itemToResult(hit)
          searchCache.set(key, item)
          return item
        }
      } catch (e) { /* 换下一个源 */ }
    }
    failCache.set(key, true)
    return null
  })()
  inflight.set(key, task)
  try { return await task } finally { inflight.delete(key) }
}

// ===== 路由 =====
const router = createRouter()

// 全局搜索（宿主 SourceResolver fan-out，单插件预算 5s → 每源 fetch 超时 4s）
router.post('/api/search', createSearchHandler({
  search: async (keyword, page = 1, pageSize = 15) => {
    if (!keyword || !String(keyword).trim()) return []
    const cfg = await getConfig()
    const limit = Math.min(50, Math.max(1, Number(pageSize) || 15))
    const cacheKey = [keyword, page, limit, cfg.sources.join(','), cfg.base].join('|')
    const cached = searchCache.get(cacheKey)
    if (cached) return cached
    if (failCache.get('search|' + cacheKey)) return []

    const { merged, ok, failed } = await searchMusicAggregated(cfg, keyword, page, limit)
    console.log('[kwqq] search "' + keyword + '" p' + page + ' -> ' + merged.length +
      ' items (' + ok + '/' + cfg.sources.length + ' sources ok' +
      (failed ? ', ' + failed + ' failed' : '') + ')')
    if (!merged.length && failed === cfg.sources.length) failCache.set('search|' + cacheKey, true)
    searchCache.set(cacheKey, merged)
    return merged
  },
}))

// 播放直链：source_data {s,i,c} → kwqq /{s}/song/url?quality=…（320k 封顶）
// 音质档位路由（spec: songloft .scratch/audio-quality-routing D5/D6）：
//   本路由自持而不用 SDK 的 createMusicUrlHandler——npm 已发布的 SDK 2.18.0 以单参调
//   resolveUrl，context.quality 透传是 2.19.0 行为，依赖它 hint 会静默丢失（评审查实）。
//   契约与 SDK 约定一致：POST {source_data, quality?, fallback?} →
//   200 {url, headers?, source_data?, used_fallback?} / 404 {error:'source_not_available'}。
//   hint 经内联 resolveQualityHint 防御归一（合法集透传/越权归 320k）；无 hint 维持
//   cfg.quality；192k snap / per-source 白名单由 kwqq-api quality.py 单点完成。
router.post('/api/music/url', async (req) => {
  let body = {}
  if (req.body) {
    try {
      body = typeof req.body === 'string' ? JSON.parse(req.body) : (req.body || {})
    } catch (e) {
      return jsonResponse({ error: 'invalid json body' }, 400)
    }
  }
  const sd = body.source_data
  if (!sd || typeof sd !== 'object') {
    return jsonResponse({ error: 'source_data is required' }, 400)
  }
  const cfg = await getConfig()
  const resolveOnce = async (sd2) => {
    if (!sd2 || !sd2.s || !sd2.i) throw new Error('source_data.s/i required')
    const tier = resolveSourceQuality(cfg, sd2.s, body.quality)
    let path = '/' + sd2.s + '/song/url?id=' + encodeURIComponent(sd2.i) + '&quality=' + tier
    if (sd2.s === 'migu' && sd2.c) path += '&copyright=' + encodeURIComponent(sd2.c)
    const data = await kwqq(cfg, path, 10000)
    if (!data || !data.url) throw new Error('kwqq 未返回直链')
    const out = { url: data.url }
    if (data.headers && Object.keys(data.headers).length) out.headers = data.headers
    console.log('[kwqq] url ' + sd2.s + '/' + sd2.i + ' ext=' + data.ext + ' ' + data.bitrate_kbps + 'kbps tier=' + tier)
    return out
  }
  try {
    return jsonResponse(await resolveOnce(sd))
  } catch (e) { /* 落入 fallback */ }
  const fb = body.fallback
  if (fb && fb.enabled && fb.title) {
    try {
      const hit = await findHit(fb.title, fb.artist || '')
      if (hit && hit.source_data) {
        const out = await resolveOnce(hit.source_data)
        out.source_data = hit.source_data
        out.used_fallback = true
        return jsonResponse(out)
      }
    } catch (e) { /* 继续到 404 */ }
  }
  return jsonResponse({ error: 'source_not_available' }, 404)
})

// 歌词提供者（宿主对无歌词歌曲调用，预算 15s）
router.get('/lyric-search', async (req) => {
  const q = parseQuery(req.query)
  const title = q.title || '', artist = q.artist || ''
  const empty = { lyric: '' }
  if (!title) return jsonResponse(empty)
  // 缓存键带版本：旧键缓存的是「只有 lyric/tlyric」的形状，24h TTL 内会给客户端
  // 少 lxlyric 的旧结果；换键即整体失效，比等 TTL 到期干净。
  const key = 'lyric2|' + title + '|' + artist
  const cached = lyricCache.get(key)
  if (cached) return jsonResponse(cached)
  if (failCache.get(key)) return jsonResponse(empty)
  const hit = await findHit(title, artist)
  if (!hit) { failCache.set(key, true); return jsonResponse(empty) }
  const sd = hit.source_data
  const cfg = await getConfig()
  for (const s of LYRIC_SOURCES.indexOf(sd.s) >= 0 ? [sd.s].concat(LYRIC_SOURCES.filter(x => x !== sd.s)) : LYRIC_SOURCES) {
    try {
      // 换源时先回搜该源的匹配（lyric by-id 只对本源 id 有效）
      let id = sd.i, extraPath = ''
      // migu 的歌词按版权下发（copyrightId 取自搜索条目 extra）；同源命中时 source_data 已带 c
      if (s === sd.s) {
        if (s === 'migu' && sd.c) extraPath = '&copyright=' + encodeURIComponent(sd.c)
      } else {
        const d2 = await kwqq(cfg, '/' + s + '/search?keywords=' +
          encodeURIComponent((title + ' ' + artist).trim()) + '&limit=3', 5000)
        const it2 = pickBest(d2.items || [], title, artist)
        if (!it2) continue
        id = String(it2.id)
        if (s === 'migu' && it2.extra && it2.extra.copyrightId) extraPath = '&copyright=' + encodeURIComponent(it2.extra.copyrightId)
      }
      const ly = await kwqq(cfg, '/' + s + '/lyric?id=' + encodeURIComponent(id) + extraPath, 8000)
      if (ly && ly.lyric) {
        const payload = { lyric: ly.lyric }
        if (ly.tlyric) payload.tlyric = ly.tlyric
        // 逐字歌词（洛雪格式，行 [mm:ss.mmm] + <偏移ms,时长ms>文本）；无逐字的源不返回该键
        if (ly.lxlyric) payload.lxlyric = ly.lxlyric
        lyricCache.set(key, payload)
        console.log('[kwqq] lyric "' + title + '" via ' + s + ' (' + ly.lyric.length + ' chars)')
        return jsonResponse(payload)
      }
    } catch (e) { /* 换下一个源 */ }
  }
  failCache.set(key, true)
  return jsonResponse(empty)
})

// 封面提供者（宿主对无封面歌曲调用）
router.get('/cover-search', async (req) => {
  const q = parseQuery(req.query)
  const title = q.title || '', artist = q.artist || ''
  if (!title) return jsonResponse({ cover_url: '' })
  const key = 'cover|' + title + '|' + artist
  const cached = coverCache.get(key)
  if (cached) return jsonResponse(cached)
  const hit = await findHit(title, artist)
  const payload = { cover_url: (hit && hit.cover_url) || '' }
  coverCache.set(key, payload)
  return jsonResponse(payload)
})

// MV（播放页「...」菜单「歌曲视频」）：GET /mv?source=&id=&title=&artist=
// 契约：{ items: [{id, source, title, artist, cover, duration_s, url, quality}] }，
// 过滤无 url 项、按可播性排序；找不到恒为 {items: []}（不 404）。MV URL 是签名短时效
// （netease wsTime / QQ fvkey / kugou 日期路径），客户端每次点击现取——故此处不缓存。
const MV_SOURCES = ['netease', 'qq', 'kugou']   // 2026-11-06 实测可匿名取直链的三源
const MV_QUALITY_RANK = { fhd: 4, hd: 3, sd: 2, ld: 1 }

function mvItem(source, raw) {
  const it = raw || {}
  const url = String(it.url || '')
  if (!url) return null
  return {
    id: String(it.id || ''),
    source: String(it.source || source),
    title: it.title || undefined,
    artist: it.artist || undefined,
    cover: it.cover || undefined,
    duration_s: Number(it.duration_s) || 0,
    url,
    quality: it.quality || undefined,
  }
}

// 门面响应可能是「顶层单条」或「顶层 + items 多候选」：两者都收集，按 url 去重
function collectMvItems(source, data) {
  const out = []
  const push = (raw) => { const it = mvItem(source, raw); if (it) out.push(it) }
  if (!data) return out
  push(data)
  const list = Array.isArray(data.items) ? data.items : []
  for (const raw of list) push(raw)
  return out
}

// MV 反查的严格匹配：pickBest 在无命中时会退 items[0]（供封面/歌词兜底），但对 MV 会把
// 「听起来像」的歌当成同一首（实测「不存在的歌名zzz」被 QQ 模糊搜索匹配成《世界上不存在的歌》），
// 所以这里要求标题必须有关系（同名 4 / 包含 2），歌手命中只加分、不作为必要条件。
function mvMatch(items, title, artist) {
  const T = String(title || '').toLowerCase().replace(/\s+/g, '')
  const A = String(artist || '').toLowerCase().replace(/\s+/g, '')
  if (!T) return null
  let best = null, bestScore = 0
  for (const it of (items || [])) {
    if (!it || !it.id) continue
    const n = String(it.name || '').toLowerCase().replace(/\s+/g, '')
    const s = String(it.singer || '').toLowerCase().replace(/\s+/g, '')
    let score = 0
    if (n === T) score += 4
    else if (n.indexOf(T) >= 0 || T.indexOf(n) >= 0) score += 2
    if (!score) continue
    if (A && s.indexOf(A) >= 0) score += 2
    if (score > bestScore) { bestScore = score; best = it }
  }
  return best
}

router.get('/mv', async (req) => {
  const q = parseQuery(req.query)
  const source = String(q.source || '')
  const id = String(q.id || '')
  const title = String(q.title || ''), artist = String(q.artist || '')
  const cfg = await getConfig()
  const items = [], seen = {}
  const add = (list) => {
    for (const it of list) {
      if (seen[it.url]) continue
      seen[it.url] = true
      items.push(it)
    }
  }
  const ask = async (s, sid) => {
    const data = await kwqq(cfg, '/' + s + '/mv?id=' + encodeURIComponent(sid) +
      (title ? '&title=' + encodeURIComponent(title) : '') +
      (artist ? '&artist=' + encodeURIComponent(artist) : ''), 9000)
    return collectMvItems(s, data)
  }

  // 1) 本源按 id 直取（source_data.s/i 就是该源点播歌曲的平台 id）
  if (source && id && ALL_SOURCES.indexOf(source) >= 0) {
    try { add(await ask(source, id)) } catch (e) { /* 该源无 MV/不可匿名 → 交给反查 */ }
  }
  // 2) 拿不到时按 title+artist 到支持 MV 的源反查（kuwo/migu/qianqian 无匿名 MV 通道）
  if (!items.length && title) {
    for (const s of MV_SOURCES) {
      if (s === source) continue
      try {
        const d = await kwqq(cfg, '/' + s + '/search?keywords=' +
          encodeURIComponent((title + ' ' + artist).trim()) + '&limit=5', 6000)
        const hit = mvMatch(d.items || [], title, artist)
        if (!hit || !hit.id) continue
        add(await ask(s, String(hit.id)))
        if (items.length) break
      } catch (e) { /* 换下一个源 */ }
    }
  }
  items.sort((a, b) => (MV_QUALITY_RANK[b.quality] || 0) - (MV_QUALITY_RANK[a.quality] || 0))
  console.log('[kwqq] mv source=' + source + ' id=' + id + ' -> ' + items.length + ' playable')
  return jsonResponse({ items })
})

// 专辑检索（消费 kwqq-api P6 `/{source}/album`；netease 先行，其余源 501 → UI 提示）
router.get('/album', async (req) => {
  const q = parseQuery(req.query)
  const source = q.source || 'netease'
  const name = q.name || ''
  const artist = q.artist || ''
  if (!name) return jsonResponse({ error: 'name required' }, 400)
  const cfg = await getConfig()
  try {
    const data = await kwqq(cfg, '/' + source + '/album?name=' + encodeURIComponent(name) +
      (artist ? '&artist=' + encodeURIComponent(artist) : ''), 12000)
    return jsonResponse(data)
  } catch (e) {
    const msg = String(e && e.message || e)
    if (msg.indexOf('501') >= 0) return jsonResponse({ error: 'source "' + source + '" 不支持专辑检索（kwqq-api 未实现）', unsupported: true }, 501)
    return jsonResponse({ error: msg }, 502)
  }
})

// ===== 配置端点（设置页用；插件路由默认带 JWT，外部无法改写） =====
router.get('/config', async () => {
  const cfg = await getConfig()
  return jsonResponse({
    base: cfg.base,
    key: cfg.key ? '******' : '',
    has_key: !!cfg.key,
    sources: cfg.sources,
    all_sources: ALL_SOURCES,
    quality: cfg.quality,
    quality_by_source: cfg.quality_by_source || {},
  })
})
router.post('/config', async (req) => {
  let body = {}
  try { body = typeof req.body === 'string' ? JSON.parse(req.body) : (req.body || {}) } catch (e) {
    return jsonResponse({ error: 'invalid json body' }, 400)
  }
  const patch = {}
  if (typeof body.base === 'string' && body.base.trim()) patch.base = body.base.trim().replace(/\/+$/, '')
  if (typeof body.key === 'string') patch.key = body.key.trim()
  if (Array.isArray(body.sources)) patch.sources = body.sources.filter(s => ALL_SOURCES.indexOf(s) >= 0)
  if (typeof body.quality === 'string' && body.quality.trim()) patch.quality = body.quality.trim()
  if (body.quality_by_source !== undefined) patch.quality_by_source = normalizeQualityBySource(body.quality_by_source)
  const cfg = await saveConfig(patch)
  console.log('[kwqq] config updated: base=' + cfg.base + ' sources=' + cfg.sources.join(','))
  return jsonResponse({ success: true, config: { base: cfg.base, sources: cfg.sources, quality: cfg.quality, quality_by_source: cfg.quality_by_source, has_key: !!cfg.key } })
})

// ===== 导入宿主曲库（插件 UI 播放的前置：宿主播放器只认数字 song id） =====
// UI 流程：搜索（本插件 /api/search）→ POST /import（此处带插件 JWT 写宿主 songs/remote，
// 按 entryPath+dedup_key 去重）→ UI 拿到宿主 song id → player.setQueue([id]) 走宿主播放器。
router.post('/import', async (req) => {
  let body = {}
  try { body = typeof req.body === 'string' ? JSON.parse(req.body) : (req.body || {}) } catch (e) {
    return jsonResponse({ error: 'invalid json body' }, 400)
  }
  const items = body.items
  if (!Array.isArray(items) || !items.length) return jsonResponse({ error: 'items required' }, 400)
  let token, host
  try {
    token = await songloft.plugin.getToken()
    host = (await songloft.plugin.getHostUrl()).replace(/\/+$/, '')
  } catch (e) {
    return jsonResponse({ error: 'host bridge unavailable: ' + String(e) }, 500)
  }
  const payload = items.slice(0, 100).map((it) => {
    const sd = it.source_data || {}
    return {
      title: it.title || sd.n || 'Unknown',
      artist: it.artist || sd.a || 'Unknown',
      album: it.album || sd.al || '',
      cover_url: it.cover_url || '',
      duration: Number(it.duration || sd.d) || 0,
      plugin_entry_path: 'kwqq',
      source_data: JSON.stringify(sd),
      dedup_key: 'kwqq_' + sd.s + '_' + sd.i,
    }
  })
  const resp = await withTimeout(fetch(host + '/api/v1/songs/remote', {
    method: 'POST',
    headers: { 'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  }), 20000, 'host import')
  if (!resp.ok) return jsonResponse({ error: 'host /songs/remote http ' + resp.status }, 502)
  const j = await resp.json()
  const songs = (j.songs || []).map((s) => ({ id: s.id, title: s.title, artist: s.artist, cover_url: s.cover_url }))
  console.log('[kwqq] imported ' + songs.length + ' songs (ids: ' + songs.map(s => s.id).join(',') + ')')
  return jsonResponse({ songs, count: j.count })
})

// ===== 在线浏览（player 曲库 MusicFree 式，spec: musicfree-style-library） =====
// 逐源能力（musicdl API-REFERENCE §4/§5.1，2026-10-29 v1.12）：专辑/歌手/歌单/榜单/推荐/评论
// 走 netease 透传（形状已实测）；专辑另支持精装 /{source}/album（netease/kugou/migu/qianqian ✅，
// kuwo/qq 501）。music 类型沿用 /api/search 的多源聚合。所有音乐条目自带 dedup_key
// （与 /import 同式：'kwqq_' + s + '_' + i），player 点播直接透传 /songs/remote，不在客户端拼格式。

// 专辑精装检索支持源（P6：2026-11-03 v1.13 起 kuwo/qq 也接入，六源全通）
const BROWSE_SOURCES_WITH_ALBUM = ['netease', 'kugou', 'migu', 'qianqian', 'kuwo', 'qq']
const NETEASE_SEARCH_TYPE = { album: 10, artist: 100, sheet: 1000 }

// —— P9 门面契约映射：server 归一形状（SearchItem/PlaylistEntry/AlbumEntry）→ browse 契约 ——
// SearchItem → browse track（source_data 由 itemToResult 拼，dedup_key 统一 kwqq_<source>_<id>）
function itemToTrack(it) {
  const r = itemToResult(it)
  r.source = it.source
  r.dedup_key = 'kwqq_' + it.source + '_' + it.id
  return r
}
// PlaylistEntry → browse sheet（meta.extra 携带平台专有字段）
function entryToSheet(it) {
  return {
    id: String(it.id), name: it.name || '', creator: it.creator || '',
    cover_url: it.cover || undefined,
    track_count: it.track_count || 0, play_count: it.play_count || 0,
    description: (it.extra && it.extra.description) || undefined,
    source: it.source,
  }
}
// 上游 publish_time 是 'YYYY-MM-DD' 字符串，客户端契约是毫秒时间戳（num）。
// 必须在此换算：字符串会让客户端 `as num?` 抛类型错误，整个专辑列表解析失败
// （现象：单曲正常、专辑页报错——QQ/酷我均命中）。
function publishTimeMs(v) {
  if (v === undefined || v === null || v === '') return undefined
  if (typeof v === 'number') return v
  const n = Number(v)
  if (!isNaN(n)) return n
  const t = Date.parse(String(v))
  return isNaN(t) ? undefined : t
}
// AlbumEntry → browse album
function entryToAlbum(it) {
  return {
    id: String(it.id), name: it.name || '', artist: it.artist || '',
    cover_url: it.cover || undefined,
    track_count: it.size || it.track_count || 0,
    publish_time: publishTimeMs(it.extra && it.extra.publish_time),
    company: (it.extra && it.extra.company) || undefined,
    description: (it.extra && it.extra.description) || undefined,
    source: it.source,
  }
}


// 通用 POST body 解析（对齐 /api/music/url 风格）
function parseBody(req) {
  if (!req.body) return {}
  try {
    return typeof req.body === 'string' ? JSON.parse(req.body) : (req.body || {})
  } catch (e) {
    return null
  }
}
function bodyError() {
  return jsonResponse({ error: 'invalid json body' }, 400)
}

// 浏览端点包装：cacheKey 非空时走 browseCache（自持缓存的端点传 null）；异常按
// 501 能力缺失 / 502 上游错误归一。501 判定：e.unsupported 显式标记优先，
// 上游原样 501（message 含 '501'）兜底——两通道都归一成 501 envelope；
// unsupportedCode 给该端点一个可机读的能力标识（如 artist 的 'unsupportedArtist'）。
async function browseHandler(cacheKey, fn, unsupportedCode) {
  if (cacheKey !== null) {
    const cached = browseCache.get(cacheKey)
    if (cached) return jsonResponse(cached)
  }
  try {
    const out = await fn()
    if (cacheKey !== null) browseCache.set(cacheKey, out)
    return jsonResponse(out)
  } catch (e) {
    const msg = String((e && e.message) || e)
    console.log('[kwqq] browse error: ' + msg)
    if ((e && e.unsupported) || msg.indexOf('501') >= 0) {
      return jsonResponse({ error: '该源暂不支持此能力', unsupported: true, code: unsupportedCode || undefined }, 501)
    }
    return jsonResponse({ error: msg }, 502)
  }
}


// 多类型搜索：music 走多源聚合（复用 /api/search 逻辑），album/artist/sheet 走 netease cloudsearch
// P9 统一门面 /{source}/search/multi（netease/kugou P9 收编 + qq/kuwo/migu/qianqian P7 精装）；
// 501 由调用方 catch 跳过。顺序 = 结果交错顺序（netease/kugou 在前维持既有曝光排序）
const SERVER_MULTI_SOURCES = ['netease', 'kugou', 'qq', 'kuwo', 'migu', 'qianqian']

async function browseSearch(body) {
  const cfg = await getConfig()
  const keywords = String(body.keywords || '').trim()
  const type = ['music', 'album', 'artist', 'sheet'].indexOf(body.type) >= 0 ? body.type : 'music'
  const page = Math.max(1, Number(body.page) || 1)
  const limit = Math.min(50, Math.max(1, Number(body.limit) || 20))
  if (!keywords) return { type, page, limit, isEnd: true, items: [] }

  if (type === 'music') {
    const { merged } = await searchMusicAggregated(cfg, keywords, page, limit)
    return { type, page, limit, isEnd: merged.length < limit ? true : undefined, items: merged }
  }

  // 类型搜索逐源 fan-out（P9 后全部走统一门面 /{source}/search/multi，P7+P9 归一）：
  //   netease/kugou（P9 收编）+ qq/kuwo/migu/qianqian（P7 精装）；bilibili 无类型通道跳过；
  //   migu/qianqian 的 sheet 上游 501 → 该源静默跳过（其余源仍有结果）。轮询交错合并。
  const order = []
  const tasks = []
  for (const src of SERVER_MULTI_SOURCES) {
    if (cfg.sources.indexOf(src) < 0) continue
    order.push(src)
    tasks.push(serverMultiSearch(cfg, src, keywords, type, page, limit).catch(() => ({ items: [], total: null })))
  }
  const per = await Promise.all(tasks)
  const lists = per.map(r => r.items).filter(l => l.length)
  const merged = []
  for (let i = 0; i < limit; i++) {
    for (const l of lists) if (l[i]) merged.push(l[i])
  }
  return {
    type, page, limit,
    isEnd: merged.length < limit ? true : undefined,
    items: merged,
    sources: per.map((r, i) => ({ source: order[i], count: (r.items || []).length }))
      .filter(x => x.count > 0).map(x => x.source),
  }
}


async function serverMultiSearch(cfg, source, keywords, type, page, limit) {
  const data = await kwqq(cfg, '/' + source + '/search/multi?keywords=' + encodeURIComponent(keywords) +
    '&type=' + type + '&page=' + page + '&limit=' + limit, 10000)
  const items = ((data && data.items) || []).map(it => {
    const out = Object.assign({}, it)
    if (out.cover) { out.cover_url = out.cover; delete out.cover }
    if (out.avatar) { out.avatar_url = out.avatar; delete out.avatar }
    out.source = source
    return out
  })
  return { items, total: (data && data.total) != null ? data.total : null }
}

// 多源聚合主体（/api/search 与 /browse/search 的 music 类型共用）：
// 每源条目统一补 source/dedup_key，轮询交错合并保证多源在前排有曝光；
// 返回 {merged, ok, failed} 供调用方做缓存/日志/失败标记。
async function searchMusicAggregated(cfg, keyword, page, limit) {
  const per = await Promise.allSettled(cfg.sources.map(async (s) => {
    const data = await kwqq(cfg, '/' + s + '/search?keywords=' +
      encodeURIComponent(keyword) + '&page=' + (Number(page) || 1) + '&limit=' + limit, 4000)
    return (data.items || []).map(it => {
      const r = itemToResult(it)
      r.source = it.source
      r.dedup_key = 'kwqq_' + it.source + '_' + it.id
      return r
    })
  }))
  if (per[0] && per[0].status === 'rejected') {
    console.log('[kwqq] first source error: ' + String((per[0].reason && per[0].reason.message) || per[0].reason))
  }
  const lists = per.filter(r => r.status === 'fulfilled').map(r => r.value).filter(l => l.length)
  const merged = []
  for (let i = 0; i < limit; i++) {
    for (const l of lists) if (l[i]) merged.push(l[i])
  }
  const failed = per.filter(r => r.status === 'rejected').length
  return { merged, ok: lists.length, failed }
}

// 专辑详情：netease 有 id 走透传（精确）；否则精装二段式 /{source}/album?name=&artist=
async function albumInfo(body) {
  const cfg = await getConfig()
  const source = String(body.source || 'netease')
  const id = body.id ? String(body.id) : ''
  const name = String(body.name || '').trim()
  const artist = String(body.artist || '').trim()

  if (id && (source === 'netease' || source === 'kugou')) {
    // P9b by-id 精确通道：官方曲序（track_no），元数据在 extra
    const data = await kwqq(cfg, '/' + source + '/album/detail?id=' + encodeURIComponent(id), 12000)
    const al0 = ((data && data.albums) || [])[0] || {}
    const tracks = ((data && data.items) || []).map(itemToTrack)
    for (let i = 0; i < tracks.length; i++) tracks[i].track_no = tracks[i].track_no || (i + 1)
    return {
      album: {
        id: String(al0.id || id),
        name: al0.name || name,
        artist: al0.artist || '',
        cover_url: al0.cover || undefined,
        publish_time: publishTimeMs(al0.extra && al0.extra.publish_time),
        company: (al0.extra && al0.extra.company) || undefined,
        description: (al0.extra && al0.extra.description) || undefined,
        track_count: (data && data.track_count) || undefined,
        source,
      },
      tracks,
    }
  }

  if (!name) return { error: 'name required（该源无 id 精确检索能力，需专辑名）' }
  if (BROWSE_SOURCES_WITH_ALBUM.indexOf(source) < 0) {
    const err = new Error('kwqq http 501')
    err.unsupported = true   // 显式能力缺失标记，browseHandler 据此归一 501 envelope
    throw err
  }
  const data = await kwqq(cfg, '/' + source + '/album?name=' + encodeURIComponent(name) +
    (artist ? '&artist=' + encodeURIComponent(artist) : ''), 12000)
  const candidates = (data && data.albums) || []
  const tracks = ((data && data.items) || []).map(itemToTrack)
  const al0 = candidates[0] || {}
  return {
    album: {
      id: al0.id ? String(al0.id) : undefined,
      name: (data && data.name) || name,
      artist: (data && data.artist) || artist,
      cover_url: al0.cover || undefined,
      track_count: (data && data.track_count) || undefined,
      source: source,
      candidates: candidates.length > 1
        ? candidates.map(c => ({ id: c.id ? String(c.id) : undefined, name: c.name, artist: c.artist, cover_url: c.cover }))
        : undefined,
    },
    tracks,
  }
}


// 歌手详情 + 作品：统一门面 /{source}/artist|artist/works。
// 能力边界由门面裁决（adapters/base 缺省实现 501 → 插件归一 501 envelope），
// 插件不再硬编码「仅 netease/kugou」——上游有通道的源自动可用。
async function artistInfo(body) {
  const id = String(body.id || '').trim()
  const source = String(body.source || 'netease')
  if (!id) return { error: 'id required' }
  const cfg = await getConfig()
  const data = await kwqq(cfg, '/' + source + '/artist?id=' + encodeURIComponent(id), 10000)
  const ar = (data && data.artist) || {}
  return {
    artist: {
      id: String(ar.id || id),
      name: ar.name || '',
      avatar_url: ar.avatar || undefined,
      alias: ar.alias || undefined,
      description: ar.description || undefined,
      album_count: ar.album_count || 0,
      music_count: ar.music_count || 0,
      source,
    },
  }
}

async function artistWorks(body) {
  const id = String(body.id || '').trim()
  const type = body.type === 'album' ? 'album' : 'music'
  const page = Math.max(1, Number(body.page) || 1)
  const limit = Math.min(50, Math.max(1, Number(body.limit) || 30))
  if (!id) return { error: 'id required' }
  const cfg = await getConfig()
  const source = String(body.source || 'netease')
  const data = await kwqq(cfg, '/' + source + '/artist/works?id=' + encodeURIComponent(id) +
    '&type=' + type + '&page=' + page + '&limit=' + limit, 10000)
  if (type === 'music') {
    return { type, page, isEnd: !!(data && data.is_end), items: ((data && data.items) || []).map(itemToTrack) }
  }
  const items = ((data && data.items) || []).map(it => ({
    id: String(it.id), name: it.name || '', artist: it.artist || '',
    cover_url: it.cover || undefined,
    track_count: it.track_count || it.size || 0,
    publish_time: publishTimeMs(it.extra && it.extra.publish_time),
    company: (it.extra && it.extra.company) || undefined,
    description: (it.extra && it.extra.description) || undefined,
    source: it.source,
  }))
  return { type, page, isEnd: !!(data && data.is_end), items }
}


// —— kuwo 浏览归一（P8：走 kwqq-api 统一门面 /kuwo/*，归一在 adapter 内完成；
// 形状 2026-11-04 现网实测。曲目 id=rid 与播放链口径一致）——
async function kuwoToplists(cfg) {
  const data = await kwqq(cfg, '/kuwo/toplists', 10000)
  return (data && data.items || []).map(it => ({
    id: it.id,
    name: it.name || '',
    cover_url: it.cover || undefined,
    description: it.description || undefined,
    source: 'kuwo',
  }))
}

async function kuwoToplistDetail(cfg, id, page, limit) {
  const data = await kwqq(cfg, '/kuwo/toplist/songs?id=' + encodeURIComponent(id) +
    '&page=' + page + '&limit=' + limit, 20000)
  const tracks = ((data && data.items) || []).map(itemToResult)
  for (const t of tracks) { t.source = 'kuwo'; t.dedup_key = 'kwqq_kuwo_' + t.source_data.i }
  return { page, isEnd: !!(data && data.is_end), total: (data && data.total) || undefined, tracks }
}

async function kuwoSheetInfo(cfg, id, page, limit) {
  const data = await kwqq(cfg, '/kuwo/playlist/songs?id=' + encodeURIComponent(id) +
    '&page=' + page + '&limit=' + limit, 20000)
  const tracks = ((data && data.items) || []).map(itemToResult)
  for (const t of tracks) { t.source = 'kuwo'; t.dedup_key = 'kwqq_kuwo_' + t.source_data.i }
  const out = {
    page, isEnd: !!(data && data.is_end), total: (data && data.total) || undefined,
    tracks,
  }
  const meta = data && data.meta
  if (page === 1 && meta) {
    out.sheet = {
      id: String(meta.id || id),
      name: meta.name || '',
      creator: meta.creator || '',
      cover_url: meta.cover || undefined,
      track_count: meta.track_count || 0,
      play_count: (meta.extra && meta.extra.play_count) || 0,
      description: (meta.extra && meta.extra.description) || undefined,
      source: 'kuwo',
    }
  }
  return out
}

// 推荐标签：pinned + 分组，TagEntry {id,title} 与 browse 契约同形，原样透传
async function kuwoRecommendTags(cfg) {
  const data = await kwqq(cfg, '/kuwo/playlist/tags', 10000)
  return { pinned: (data && data.pinned) || [], groups: (data && data.groups) || [] }
}

async function kuwoRecommendSheets(cfg, cat, page, limit) {
  const data = await kwqq(cfg, '/kuwo/playlist/sheets?tag=' + encodeURIComponent(cat) +
    '&page=' + page + '&limit=' + limit, 20000)
  return {
    cat, page,
    isEnd: !!(data && data.is_end),
    items: ((data && data.items) || []).map(it => ({
      id: it.id,
      name: it.name || '',
      creator: it.creator || '',
      cover_url: it.cover || undefined,
      track_count: it.track_count || 0,
      play_count: it.play_count || 0,
      source: 'kuwo',
    })),
  }
}
// 歌单/榜单详情：P8 门面 /{source}/playlist/songs（meta 仅 page=1 附元数据）。
// 平台顺序统一为 网易云 / QQ / 酷狗 / 酷我（与 App 端展示一致）。
const BROWSABLE_SOURCES = ['netease', 'qq', 'kugou', 'kuwo']
const SHEET_SOURCES = BROWSABLE_SOURCES
async function sheetInfo(body) {
  const id = String(body.id || '').trim()
  const page = Math.max(1, Number(body.page) || 1)
  const limit = Math.min(100, Math.max(1, Number(body.limit) || 50))
  if (!id) return { error: 'id required' }
  const cfg = await getConfig()
  const source = String(body.source || 'netease')
  if (SHEET_SOURCES.indexOf(source) < 0) {
    const err = new Error('source "' + source + '" 不支持歌单详情（仅 netease/qq/kugou/kuwo）')
    err.unsupported = true
    throw err
  }
  const data = await kwqq(cfg, '/' + source + '/playlist/songs?id=' + encodeURIComponent(id) +
    '&page=' + page + '&limit=' + limit, 15000)
  const out = {
    page, isEnd: !!(data && data.is_end), total: (data && data.total) || undefined,
    tracks: ((data && data.items) || []).map(itemToTrack),
  }
  if (page === 1 && data && data.meta) out.sheet = entryToSheet(data.meta)
  return out
}


// 榜单列表：P8 门面多源聚合（网易云/QQ/酷狗/酷我），items 各带 source，单源失败静默跳过
async function toplists() {
  const cfg = await getConfig()
  const tasks = []
  for (const s of BROWSABLE_SOURCES) {
    if (cfg.sources.indexOf(s) < 0) continue
    tasks.push(kwqq(cfg, '/' + s + '/toplists', 10000).then(data =>
      ((data && data.items) || []).map(it => ({
        id: it.id, name: it.name || '',
        cover_url: it.cover || undefined,
        description: it.description || undefined,
        update_frequency: it.update_frequency || undefined,
        source: s,
      }))))
  }
  const per = await Promise.allSettled(tasks)
  const items = []
  let lastErr = null
  for (const r of per) {
    if (r.status === 'fulfilled') items.push(...r.value)
    else {
      lastErr = r.reason
      console.log('[kwqq] toplists one-source error: ' + String((r.reason && r.reason.message) || r.reason))
    }
  }
  if (!items.length && lastErr) throw lastErr // 全部源失败才归一 502
  return { source: 'multi', items }
}

async function toplistDetail(body) {
  const id = String(body.id || '').trim()
  const page = Math.max(1, Number(body.page) || 1)
  const limit = Math.min(100, Math.max(1, Number(body.limit) || 50))
  if (!id) return { error: 'id required' }
  const cfg = await getConfig()
  const source = String(body.source || 'netease')
  if (SHEET_SOURCES.indexOf(source) < 0) {
    const err = new Error('source "' + source + '" 不支持榜单详情（仅 netease/qq/kugou/kuwo）')
    err.unsupported = true
    throw err
  }
  const data = await kwqq(cfg, '/' + source + '/toplist/songs?id=' + encodeURIComponent(id) +
    '&page=' + page + '&limit=' + limit, 15000)
  return { page, isEnd: !!(data && data.is_end), total: (data && data.total) || undefined,
           tracks: ((data && data.items) || []).map(itemToTrack) }
}


// 推荐歌单：P8 门面 /{source}/playlist/tags 四平台（netease/qq/kugou/kuwo）。
// 顶层 pinned/categories 保持第一平台（netease 优先，旧客户端兼容）；
// platforms 数组供新客户端按平台展示。
async function recommendTags() {
  const cfg = await getConfig()
  const platforms = []
  for (const s of BROWSABLE_SOURCES) {
    if (cfg.sources.indexOf(s) < 0) continue
    try {
      const data = await kwqq(cfg, '/' + s + '/playlist/tags', 10000)
      const groups = (data && data.groups) || []
      if (groups.length) platforms.push({ source: s, pinned: (data && data.pinned) || [], categories: groups })
    } catch (e) {
      console.log('[kwqq] ' + s + ' recommend tags error: ' + String((e && e.message) || e))
    }
  }
  const first = platforms.find(p => p.source === 'netease') || platforms[0] || { pinned: [], categories: [] }
  return { pinned: first.pinned, categories: first.categories, platforms }
}

// 推荐歌单：P8 门面 /{source}/playlist/sheets（tag 空 = 全部热门；kuwo 专区 <id>@<digest> 透传）
async function recommendSheets(body) {
  const cat = String(body.cat || '').trim()
  const page = Math.max(1, Number(body.page) || 1)
  const limit = Math.min(50, Math.max(1, Number(body.limit) || 30))
  const cfg = await getConfig()
  const source = String(body.source || 'netease')
  if (SHEET_SOURCES.indexOf(source) < 0) {
    const err = new Error('source "' + source + '" 不支持推荐歌单（仅 netease/qq/kugou/kuwo）')
    err.unsupported = true
    throw err
  }
  const data = await kwqq(cfg, '/' + source + '/playlist/sheets?tag=' + encodeURIComponent(cat) +
    '&page=' + page + '&limit=' + limit, 10000)
  return {
    cat, page,
    isEnd: !!(data && data.is_end),
    items: ((data && data.items) || []).map(entryToSheet),
  }
}


// 评论（只读，MusicFree IComment 形状；热评首页随 hotComments 一次返回）。
// id 可省：给 title+artist 时回搜 netease id（播放页对任意来源的已入库歌开评论用）。
async function musicComments(body) {
  let id = String(body.id || '').trim()
  const title = String(body.title || '').trim()
  const artist = String(body.artist || '').trim()
  const page = Math.max(1, Number(body.page) || 1)
  const limit = Math.min(50, Math.max(1, Number(body.limit) || 20))
  if (!id && !title) return { error: 'id 或 title 至少提供一个' }
  const cfg = await getConfig()
  // P9 门面：/netease/comments 支持 id 或 title+artist 回搜（回搜在 server 侧完成）
  const cacheKey = 'comments|' + id + '|' + page + '|' + limit
  const cached = commentCache.get(cacheKey)
  if (cached) return cached
  const data = await kwqq(cfg, '/netease/comments?' + (id ? 'id=' + encodeURIComponent(id) : 'title=' + encodeURIComponent(title) + '&artist=' + encodeURIComponent(artist)) +
    '&page=' + page + '&limit=' + limit, 10000)
  const toComment = (c) => {
    if (!c) return null
    // P9 门面 CommentEntry 形状（snake_case）→ MusicFree IComment（camelCase）
    return {
      id: c.id || '',
      nickName: c.nick_name || '匿名用户',
      avatar: c.avatar || undefined,
      comment: c.content || '',
      like: c.like || 0,
      createAt: c.created_at || undefined,
      replies: undefined,
    }
  }
  const out = {
    page,
    total: (data && data.total) || 0,
    has_more: !!(data && data.has_more),
    hot: (((data && data.hot) || []).map(toComment).filter(Boolean)),
    comments: (((data && data.items) || []).map(toComment).filter(Boolean)),
  }
  commentCache.set(cacheKey, out)
  return out
}

// 浏览路由注册（表驱动：全部 POST + JSON body，对齐 /api/music/url 风格；player 经
// /api/v1/jsplugin/kwqq/* 调用，宿主 catch-all 已带 JWT 鉴权与透传）。
// 每行：path / validate(body)→错误文案或 null / cacheKey(body)→null 表示 handler 自持缓存 /
// run(body)→业务处理（browseSearch 等上文 async 函数）。
const BROWSE_ROUTES = [
  {
    path: '/browse/search',
    validate: (b) => (String(b.keywords || '').trim() ? null : 'keywords required'),
    cacheKey: (b) => 'bs|' + String(b.type || 'music') + '|' + String(b.keywords || '').trim() +
      '|' + (Number(b.page) || 1) + '|' + (Number(b.limit) || 20),
    run: (b) => browseSearch(b),
  },
  {
    path: '/album/info',
    validate: (b) => (((b.id && String(b.id).trim()) || String(b.name || '').trim()) ? null : 'id 或 name 至少提供一个'),
    unsupported: (b) => {
      const source = String(b.source || 'netease')
      return source !== 'netease' && BROWSE_SOURCES_WITH_ALBUM.indexOf(source) < 0
    },
    cacheKey: (b) => 'al|' + String(b.source || 'netease') + '|' + (b.id || '') + '|' +
      String(b.name || '') + '|' + String(b.artist || ''),
    run: (b) => albumInfo(b),
  },
  {
    path: '/artist/info',
    validate: (b) => (String(b.id || '').trim() ? null : 'id required'),
    cacheKey: (b) => 'ar|' + String(b.source || 'netease') + '|' + String(b.id || ''),
    code: 'unsupportedArtist',
    run: (b) => artistInfo(b),
  },
  {
    path: '/artist/works',
    validate: (b) => (String(b.id || '').trim() ? null : 'id required'),
    cacheKey: (b) => 'arw|' + String(b.source || 'netease') + '|' + String(b.id || '') + '|' + (b.type || 'music') + '|' +
      (Number(b.page) || 1) + '|' + (Number(b.limit) || 30),
    code: 'unsupportedArtist',
    run: (b) => artistWorks(b),
  },
  {
    path: '/sheet/info',
    validate: (b) => (String(b.id || '').trim() ? null : 'id required'),
    cacheKey: (b) => 'sh|' + String(b.source || 'netease') + '|' + String(b.id || '') + '|' +
      (Number(b.page) || 1) + '|' + (Number(b.limit) || 50),
    run: (b) => sheetInfo(b),
  },
  {
    path: '/toplists',
    validate: null,
    cacheKey: () => 'tls',
    run: () => toplists(),
  },
  {
    path: '/toplist/detail',
    validate: (b) => (String(b.id || '').trim() ? null : 'id required'),
    cacheKey: (b) => 'tld|' + String(b.source || 'netease') + '|' + String(b.id || '') + '|' +
      (Number(b.page) || 1) + '|' + (Number(b.limit) || 50),
    run: (b) => toplistDetail(b),
  },
  {
    path: '/recommend/tags',
    validate: null,
    cacheKey: () => 'rtags',
    run: () => recommendTags(),
  },
  {
    path: '/recommend/sheets',
    validate: null,
    cacheKey: (b) => 'rsh|' + String(b.source || 'netease') + '|' + String(b.cat || '') + '|' +
      (Number(b.page) || 1) + '|' + (Number(b.limit) || 30),
    run: (b) => recommendSheets(b),
  },
  {
    // 评论自持 commentCache（60s，热评随点赞变动），不走 browseCache
    path: '/music/comments',
    validate: (b) => ((String(b.id || '').trim() || String(b.title || '').trim()) ? null : 'id 或 title 至少提供一个'),
    cacheKey: () => null,
    run: (b) => musicComments(b),
  },
]

function registerBrowseRoutes(router) {
  for (const route of BROWSE_ROUTES) {
    router.post(route.path, async (req) => {
      const body = parseBody(req)
      if (body === null) return bodyError()
      if (route.validate) {
        const err = route.validate(body)
        if (err) return jsonResponse({ error: err }, 400)
      }
      // 能力缺失类（不支持源）→ 501 envelope，不进缓存
      if (route.unsupported && route.unsupported(body)) {
        return jsonResponse({ error: '该源暂不支持此能力', unsupported: true, code: route.code || undefined }, 501)
      }
      return browseHandler(route.cacheKey ? route.cacheKey(body) : null, () => route.run(body), route.code)
    })
  }
}

registerBrowseRoutes(router)

// ===== 生命周期 =====
async function onInit() {
  console.log('[kwqq] mounted')
  try { songloft.lyrics.registerProvider(); console.log('[kwqq] lyric provider registered') }
  catch (e) { console.error('[kwqq] lyrics.registerProvider failed: ' + String(e)) }
  try {
    const covers = songloft && songloft.covers
    if (covers && typeof covers.registerProvider === 'function') {
      covers.registerProvider(); console.log('[kwqq] cover provider registered')
    }
  } catch (e) { /* 旧宿主无 covers，忽略 */ }
}

async function onDeinit() {
  try { songloft.lyrics.unregisterProvider() } catch (e) { /* ignore */ }
  try {
    const covers = songloft && songloft.covers
    if (covers && typeof covers.unregisterProvider === 'function') covers.unregisterProvider()
  } catch (e) { /* ignore */ }
  console.log('[kwqq] unmounted')
}

async function onHTTPRequest(req) {
  return await router.handle(req)
}

globalThis.onInit = onInit
globalThis.onDeinit = onDeinit
globalThis.onHTTPRequest = onHTTPRequest
