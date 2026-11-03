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

const ALL_SOURCES = ['kuwo', 'qq', 'qianqian', 'migu', 'netease', 'kugou', 'bilibili']
// 歌词回搜顺序：酷我歌词链路最稳（qianqian by-id 不支持歌词，跳过）
const LYRIC_SOURCES = ['kuwo', 'qq', 'netease', 'migu', 'kugou']

// 默认源 = 六个 mp3 源。bilibili 在设置页手动启用（默认关）：
//   App 内试听可用（宿主代理会带 Referer/UA 拉 fMP4/m4s），但 **DLNA 投送不可用**（音箱解不了 fMP4 分片）。
const DEFAULT_SOURCES = ['kuwo', 'qq', 'qianqian', 'migu', 'netease', 'kugou']

const DEFAULTS = {
  base: 'http://10.10.10.2:3003',
  key: '',
  sources: DEFAULT_SOURCES,
  quality: '320k',
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
  return cfgCache
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
  const tier = resolveQualityHint(body.quality, cfg.quality || DEFAULT_QUALITY_TIER)
  const resolveOnce = async (sd2) => {
    if (!sd2 || !sd2.s || !sd2.i) throw new Error('source_data.s/i required')
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
  const key = 'lyric|' + title + '|' + artist
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
      if (s !== sd.s) {
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
  const cfg = await saveConfig(patch)
  console.log('[kwqq] config updated: base=' + cfg.base + ' sources=' + cfg.sources.join(','))
  return jsonResponse({ success: true, config: { base: cfg.base, sources: cfg.sources, quality: cfg.quality, has_key: !!cfg.key } })
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

// 透传返回的 netease song 对象（al/ar/dt 形状）→ 播放条目（对齐 itemToResult 输出形状）
function neteaseSongToResult(song) {
  const albumName = (song.al && song.al.name) || (song.album && song.album.name) || ''
  const artist = (song.ar || song.artists || []).map(a => a.name).filter(Boolean).join(' / ')
  const cover = (song.al && song.al.picUrl) || (song.album && song.album.picUrl) || ''
  const sd = { s: 'netease', i: String(song.id), n: song.name || '', a: artist, al: albumName, d: Math.round((song.dt || 0) / 1000) }
  return {
    title: song.name || 'Unknown',
    artist: artist || 'Unknown',
    album: albumName || undefined,
    duration: sd.d,
    cover_url: cover || undefined,
    source: 'netease',
    source_data: sd,
    dedup_key: 'kwqq_netease_' + song.id,
  }
}

// netease 透传封面加尺寸参数（MusicFree 同款 ?param=WxH）
function neteaseCover(url, size) {
  if (!url) return undefined
  return url + '?param=' + (size || 300) + 'y' + (size || 300)
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
// 上游原样 501（message 含 '501'）兜底——两通道都归一成 501 envelope。
async function browseHandler(cacheKey, fn) {
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
      return jsonResponse({ error: '该源暂不支持此能力', unsupported: true }, 501)
    }
    return jsonResponse({ error: msg }, 502)
  }
}

// netease 透传调用：透传返回的是上游原样 JSON（无 {code,msg,data} 信封），
// 不能走 kwqq()（它会拆信封返回 j.data → undefined），必须自持裸 fetch。
async function neteaseApi(cfg, path, timeoutMs) {
  const url = cfg.base.replace(/\/+$/, '') + '/netease/api/' + path
  const headers = {}
  if (cfg.key) headers['X-API-Key'] = cfg.key
  const resp = await withTimeout(fetch(url, { headers }), timeoutMs || 10000, 'netease')
  if (resp.status === 401) throw new Error('kwqq 401：API key 缺失或错误')
  if (!resp.ok) throw new Error('kwqq http ' + resp.status)
  const j = await resp.json()
  if (j && typeof j.code === 'number' && j.code !== 200) throw new Error('netease code ' + j.code)
  return j
}

// 多类型搜索：music 走多源聚合（复用 /api/search 逻辑），album/artist/sheet 走 netease cloudsearch
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

  // 类型搜索逐源 fan-out（musicfree-plugins 七插件现役实现对齐，2026-11-03）：
  //   netease 透传 cloudsearch type=10/100/1000；kugou 透传 /kugou/api/search（字符串枚举
  //   album/author/special，数字 type 不分流——实测确认）；qq/kuwo/migu/qianqian 经 server
  //   精装 /{source}/search/multi（P7，API-REFERENCE §4.5b）；bilibili 无类型通道跳过；
  //   migu/qianqian 的 sheet 上游 501 → 该源静默跳过（其余源仍有结果）。轮询交错合并。
  const order = ['netease']
  const tasks = [neteaseMultiSearch(cfg, keywords, type, page, limit).catch(() => ({ items: [], total: null }))]
  if (cfg.sources.indexOf('kugou') >= 0) {
    order.push('kugou')
    tasks.push(kugouMultiSearch(cfg, keywords, type, page, limit).catch(() => ({ items: [], total: null })))
  }
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

// netease 透传类型搜索（cloudsearch；形状经 NAS 实测）
async function neteaseMultiSearch(cfg, keywords, type, page, limit) {
  const offset = (page - 1) * limit
  const data = await neteaseApi(cfg, 'cloudsearch?keywords=' + encodeURIComponent(keywords) +
    '&type=' + NETEASE_SEARCH_TYPE[type] + '&limit=' + limit + '&offset=' + offset, 10000)
  const res = (data && data.result) || {}
  let items, total
  if (type === 'album') {
    total = res.albumCount || 0
    items = (res.albums || []).map(al => ({
      id: String(al.id),
      name: al.name || '',
      artist: (al.artist && al.artist.name) || ((al.artists || []).map(a => a.name).filter(Boolean).join(' / ')) || '',
      cover_url: neteaseCover(al.picUrl),
      track_count: al.size || 0,
      publish_time: al.publishTime || undefined,
      company: al.company || undefined,
      source: 'netease',
    }))
  } else if (type === 'artist') {
    total = res.artistCount || 0
    items = (res.artists || []).map(ar => ({
      id: String(ar.id),
      name: ar.name || '',
      avatar_url: neteaseCover(ar.picUrl || ar.img1v1Url, 200),
      alias: (ar.alias && ar.alias[0]) || undefined,
      album_count: ar.albumSize || 0,
      music_count: ar.musicSize || 0,
      source: 'netease',
    }))
  } else {
    total = res.playlistCount || 0
    items = (res.playlists || []).map(pl => ({
      id: String(pl.id),
      name: pl.name || '',
      creator: (pl.creator && pl.creator.nickname) || '',
      cover_url: neteaseCover(pl.coverImgUrl || pl.picUrl),
      track_count: pl.trackCount || 0,
      play_count: pl.playCount || 0,
      description: pl.description || undefined,
      source: 'netease',
    }))
  }
  return { items, total }
}

// kugou 透传类型搜索（type 字符串枚举：album/author/special；形状 2026-11-03 实测）
const KUGOU_MULTI_TYPE = { album: 'album', artist: 'author', sheet: 'special' }
async function kugouMultiSearch(cfg, keywords, type, page, limit) {
  const raw = await kwqq(cfg, '/kugou/api/search?keywords=' + encodeURIComponent(keywords) +
    '&type=' + KUGOU_MULTI_TYPE[type] + '&page=' + page + '&pagesize=' + limit, 10000)
  // 透传原样 JSON：{status, error_code, data:{total, lists:[...]}}
  const data = (raw && raw.data) || raw || {}
  const rows = data.lists || []
  let items
  if (type === 'album') {
    items = rows.filter(r => r.albumid).map(r => ({
      id: String(r.albumid),
      name: r.albumname || '',
      artist: Array.isArray(r.singers) ? r.singers.map(x => x.name).filter(Boolean).join(' / ') : (r.singer || ''),
      cover_url: r.img || undefined,
      source: 'kugou',
    }))
  } else if (type === 'artist') {
    items = rows.filter(r => r.AuthorId || r.singerid).map(r => ({
      id: String(r.AuthorId || r.singerid),
      name: r.AuthorName || r.singername || '',
      avatar_url: r.Avatar || r.avatar || undefined,
      source: 'kugou',
    }))
  } else {
    items = rows.filter(r => r.specialid || r.gid).map(r => ({
      id: String(r.specialid || r.gid),
      name: r.specialname || '',
      creator: r.nickname || '',
      cover_url: r.img || undefined,
      track_count: r.song_count || 0,
      source: 'kugou',
    }))
  }
  return { items, total: (data && data.total) || rows.length }
}

// qq/kuwo/migu/qianqian：server 精装 /{source}/search/multi（P7）；501 由调用方 catch 跳过
const SERVER_MULTI_SOURCES = ['qq', 'kuwo', 'migu', 'qianqian']
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

  if (source === 'netease' && id) {
    const data = await neteaseApi(cfg, 'album?id=' + encodeURIComponent(id), 12000)
    const al = (data && data.album) || {}
    const songs = (data && data.songs) || []
    return {
      album: {
        id: String(al.id || id),
        name: al.name || name,
        artist: (al.artist && al.artist.name) || '',
        cover_url: neteaseCover(al.picUrl, 500),
        publish_time: al.publishTime || undefined,
        company: al.company || undefined,
        description: al.description || undefined,
        track_count: al.size || songs.length,
        source: 'netease',
      },
      tracks: songs.map(s => {
        const r = neteaseSongToResult(s)
        r.track_no = s.no || undefined
        return r
      }),
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
  const tracks = ((data && data.items) || []).map(it => {
    const r = itemToResult(it)
    r.source = it.source
    r.dedup_key = 'kwqq_' + it.source + '_' + it.id
    r.track_no = it.track_no || undefined
    return r
  })
  const al0 = candidates[0] || {}
  return {
    album: {
      id: al0.id ? String(al0.id) : undefined,
      name: (data && data.name) || name,
      artist: (data && data.artist) || artist,
      cover_url: al0.cover || undefined,
      track_count: (data && data.total) || tracks.length,
      source: source,
      candidates: candidates.length > 1
        ? candidates.map(c => ({ id: c.id ? String(c.id) : undefined, name: c.name, artist: c.artist, cover_url: c.cover }))
        : undefined,
    },
    tracks,
  }
}

// 歌手详情 + 作品：netease / kugou（透传 artist/detail，2026-11-03 实测形状）；其余源 501
function unsupportedArtist(source) {
  const err = new Error('source "' + source + '" 不支持歌手详情（仅 netease/kugou 有通道）')
  err.unsupported = true
  return err
}

async function artistInfo(body) {
  const id = String(body.id || '').trim()
  const source = String(body.source || 'netease')
  if (!id) return { error: 'id required' }
  const cfg = await getConfig()
  if (source === 'kugou') {
    const data = await kwqq(cfg, '/kugou/api/artist/detail?id=' + encodeURIComponent(id), 10000)
    const ar = (data && data.data) || {}
    let description = ''
    if (Array.isArray(ar.long_intro) && ar.long_intro.length && ar.long_intro[0]) {
      description = ar.long_intro[0].content || ''
    }
    return {
      artist: {
        id: String(ar.author_id || id),
        name: ar.author_name || '',
        avatar_url: (ar.sizable_avatar || '').replace('{size}', '240') || undefined,
        description: description || undefined,
        album_count: ar.album_count || 0,
        // 现网容器实测字段是 song_count（audio_count 作旧形兼容）
        music_count: ar.song_count || ar.audio_count || 0,
        source: 'kugou',
      },
    }
  }
  if (source !== 'netease') throw unsupportedArtist(source)
  const data = await neteaseApi(cfg, 'artists?id=' + encodeURIComponent(id), 10000)
  const ar = (data && data.artist) || {}
  return {
    artist: {
      id: String(ar.id || id),
      name: ar.name || '',
      avatar_url: neteaseCover(ar.picUrl || ar.img1v1Url, 300),
      alias: (ar.alias && ar.alias[0]) || undefined,
      description: ar.briefDesc || ar.description || undefined,
      album_count: ar.albumSize || 0,
      music_count: ar.musicSize || 0,
      source: 'netease',
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
  const offset = (page - 1) * limit
  if (source === 'kugou') {
    if (type === 'music') {
      // audios 行 id 取 hash_128||hash——kwqq-api /kugou/song/url 以 FileHash 取链
      const data = await kwqq(cfg, '/kugou/api/artist/audios?id=' + encodeURIComponent(id) +
        '&page=' + page + '&pagesize=' + limit, 10000)
      const rows = (data && data.data) || []
      const total = (data && data.total) || rows.length
      const items = rows.map(r => {
        const hash = r.hash_128 || r.hash || ''
        const duration = r.timelength_128 || r.timelength
        return {
          title: r.audio_name || 'Unknown',
          artist: r.author_name || 'Unknown',
          album: r.album_name || undefined,
          duration: duration ? Math.round(Number(duration) / 1000) : 0,
          cover_url: (r.trans_param && r.trans_param.union_cover) || undefined,
          source: 'kugou',
          source_data: { s: 'kugou', i: String(hash), n: r.audio_name || '', a: r.author_name || '', al: r.album_name || '' },
          dedup_key: 'kwqq_kugou_' + hash,
        }
      }).filter(x => x.source_data.i)
      return { type, page, isEnd: page * limit >= total, items }
    }
    const data = await kwqq(cfg, '/kugou/api/artist/albums?id=' + encodeURIComponent(id) +
      '&page=' + page + '&pagesize=' + limit, 10000)
    const rows = (data && data.data) || []
    const total = (data && data.total) || rows.length
    const items = rows.filter(r => r.album_id).map(r => ({
      id: String(r.album_id),
      name: r.album_name || '',
      artist: r.author_name || '',
      cover_url: (r.sizable_cover || '').replace('{size}', '240') || undefined,
      description: r.intro || undefined,
      source: 'kugou',
    }))
    return { type, page, isEnd: page * limit >= total, items }
  }
  if (source !== 'netease') throw unsupportedArtist(source)
  if (type === 'music') {
    const data = await neteaseApi(cfg, 'artist/songs?id=' + encodeURIComponent(id) +
      '&limit=' + limit + '&offset=' + offset + '&order=hot', 10000)
    const songs = (data && data.songs) || []
    const total = (data && data.total) || songs.length
    return { type, page, isEnd: offset + songs.length >= total, items: songs.map(neteaseSongToResult) }
  }
  const data = await neteaseApi(cfg, 'artist/album?id=' + encodeURIComponent(id) +
    '&limit=' + limit + '&offset=' + offset, 10000)
  const albums = (data && data.hotAlbums) || []
  const total = ((data && data.artist && data.artist.albumSize) || albums.length)
  return {
    type, page,
    isEnd: offset + albums.length >= total,
    items: albums.map(al => ({
      id: String(al.id),
      name: al.name || '',
      artist: (al.artist && al.artist.name) || '',
      cover_url: neteaseCover(al.picUrl),
      track_count: al.size || 0,
      publish_time: al.publishTime || undefined,
      company: al.company || undefined,
      source: 'netease',
    })),
  }
}

// 歌单详情：曲目分页走 playlist/track/all，首页并拉 playlist/detail 元数据
async function sheetInfo(body) {
  const id = String(body.id || '').trim()
  const page = Math.max(1, Number(body.page) || 1)
  const limit = Math.min(100, Math.max(1, Number(body.limit) || 50))
  if (!id) return { error: 'id required' }
  const cfg = await getConfig()
  const offset = (page - 1) * limit
  const data = await neteaseApi(cfg, 'playlist/track/all?id=' + encodeURIComponent(id) +
    '&limit=' + limit + '&offset=' + offset, 15000)
  const songs = (data && data.songs) || []
  const total = (data && data.total) || songs.length
  const out = {
    page, isEnd: offset + songs.length >= total, total,
    tracks: songs.map(neteaseSongToResult),
  }
  if (page === 1) {
    try {
      const det = await neteaseApi(cfg, 'playlist/detail?id=' + encodeURIComponent(id), 10000)
      const pl = (det && det.playlist) || {}
      out.sheet = {
        id: String(pl.id || id),
        name: pl.name || '',
        creator: (pl.creator && pl.creator.nickname) || '',
        cover_url: neteaseCover(pl.coverImgUrl, 500),
        track_count: pl.trackCount || total,
        play_count: pl.playCount || 0,
        description: pl.description || undefined,
        source: 'netease',
      }
    } catch (e) { /* 元数据缺失不阻塞曲目 */ }
  }
  return out
}

// 榜单列表 + 详情（netease toplist；榜单项 id 即 playlist id，详情复用 track/all 分页）
async function toplists() {
  const cfg = await getConfig()
  const data = await neteaseApi(cfg, 'toplist', 10000)
  const lists = (data && data.list) || []
  return {
    source: 'netease',
    items: lists.map(it => ({
      id: String(it.id),
      name: it.name || '',
      cover_url: neteaseCover(it.coverImgUrl || it.picUrl),
      description: it.description || undefined,
      update_frequency: it.updateFrequency || undefined,
      track_count: it.trackCount || undefined,
      source: 'netease',
    })),
  }
}

async function toplistDetail(body) {
  const id = String(body.id || '').trim()
  const page = Math.max(1, Number(body.page) || 1)
  const limit = Math.min(100, Math.max(1, Number(body.limit) || 50))
  if (!id) return { error: 'id required' }
  const cfg = await getConfig()
  const offset = (page - 1) * limit
  const data = await neteaseApi(cfg, 'playlist/track/all?id=' + encodeURIComponent(id) +
    '&limit=' + limit + '&offset=' + offset, 15000)
  const songs = (data && data.songs) || []
  const total = (data && data.total) || songs.length
  return { page, isEnd: offset + songs.length >= total, total, tracks: songs.map(neteaseSongToResult) }
}

// 推荐歌单：热门标签（pinned）+ 全量分类（grouped），来自平台接口
async function recommendTags() {
  const cfg = await getConfig()
  const hot = await neteaseApi(cfg, 'playlist/hot', 8000)
  const pinned = (((hot && hot.tags) || [])).slice(0, 8).map(t => ({ id: t.name, title: t.name }))
  let categories = []
  try {
    const cat = await neteaseApi(cfg, 'playlist/catlist', 8000)
    const subs = (cat && cat.sub) || []
    const names = (cat && cat.categories) || {}
    const grouped = {}
    for (const s of subs) {
      const catName = names[String(s.category)] || '其他'
      if (!grouped[catName]) grouped[catName] = []
      grouped[catName].push({ id: s.name, title: s.name })
    }
    categories = Object.keys(grouped).map(title => ({ title, data: grouped[title] }))
  } catch (e) { /* catlist 失败时只给 pinned */ }
  return { pinned, categories }
}

async function recommendSheets(body) {
  const cat = String(body.cat || '').trim()
  const page = Math.max(1, Number(body.page) || 1)
  const limit = Math.min(50, Math.max(1, Number(body.limit) || 30))
  const cfg = await getConfig()
  const offset = (page - 1) * limit
  const data = await neteaseApi(cfg, 'top/playlist?cat=' + encodeURIComponent(cat) +
    '&limit=' + limit + '&offset=' + offset + '&order=hot', 10000)
  const playlists = (data && data.playlists) || []
  const total = (data && data.total) || playlists.length
  return {
    cat, page,
    isEnd: offset + playlists.length >= total,
    items: playlists.map(pl => ({
      id: String(pl.id),
      name: pl.name || '',
      creator: (pl.creator && pl.creator.nickname) || '',
      cover_url: neteaseCover(pl.coverImgUrl || pl.picUrl),
      track_count: pl.trackCount || 0,
      play_count: pl.playCount || 0,
      description: pl.description || undefined,
      source: 'netease',
    })),
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
  if (!id) {
    const found = await neteaseApi(cfg, 'cloudsearch?keywords=' +
      encodeURIComponent((title + ' ' + artist).trim()) + '&type=1&limit=1&offset=0', 8000)
    const songs = (found && found.result && found.result.songs) || []
    if (!songs.length) return { total: 0, has_more: false, hot: [], comments: [] }
    id = String(songs[0].id)
  }
  const cacheKey = 'comments|' + id + '|' + page + '|' + limit
  const cached = commentCache.get(cacheKey)
  if (cached) return cached
  const offset = (page - 1) * limit
  const data = await neteaseApi(cfg, 'comment/music?id=' + encodeURIComponent(id) +
    '&limit=' + limit + '&offset=' + offset, 10000)
  const toComment = (c) => {
    if (!c) return null
    const replies = (c.beReplied || []).map(r => ({
      id: String(r.beRepliedCommentId || ''),
      nickName: (r.user && r.user.nickname) || '已注销',
      avatar: (r.user && r.user.avatarUrl) || undefined,
      comment: r.content || '',
    }))
    return {
      id: String(c.commentId || ''),
      nickName: (c.user && c.user.nickname) || '匿名用户',
      avatar: (c.user && c.user.avatarUrl) || undefined,
      comment: c.content || '',
      like: c.likedCount || 0,
      createAt: c.time || undefined,
      replies: replies.length ? replies : undefined,
    }
  }
  const out = {
    page,
    total: (data && data.total) || 0,
    has_more: !!(data && data.more),
    hot: (((data && data.hotComments) || []).map(toComment).filter(Boolean)),
    comments: (((data && data.comments) || []).map(toComment).filter(Boolean)),
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
    cacheKey: (b) => 'ar|' + String(b.id || ''),
    run: (b) => artistInfo(b),
  },
  {
    path: '/artist/works',
    validate: (b) => (String(b.id || '').trim() ? null : 'id required'),
    cacheKey: (b) => 'arw|' + String(b.id || '') + '|' + (b.type || 'music') + '|' +
      (Number(b.page) || 1) + '|' + (Number(b.limit) || 30),
    run: (b) => artistWorks(b),
  },
  {
    path: '/sheet/info',
    validate: (b) => (String(b.id || '').trim() ? null : 'id required'),
    cacheKey: (b) => 'sh|' + String(b.id || '') + '|' + (Number(b.page) || 1) + '|' + (Number(b.limit) || 50),
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
    cacheKey: (b) => 'tld|' + String(b.id || '') + '|' + (Number(b.page) || 1) + '|' + (Number(b.limit) || 50),
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
    cacheKey: (b) => 'rsh|' + String(b.cat || '') + '|' + (Number(b.page) || 1) + '|' + (Number(b.limit) || 30),
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
        return jsonResponse({ error: '该源暂不支持此能力', unsupported: true }, 501)
      }
      return browseHandler(route.cacheKey ? route.cacheKey(body) : null, () => route.run(body))
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
