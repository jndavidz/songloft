// kwqq 音源插件 —— 包装自建 kw-qq-music-api（kwqq）
// 契约：POST /api/search、POST /api/music/url（@songloft/plugin-sdk 约定）；
//       GET /lyric-search、GET /cover-search（宿主歌词/封面提供者，onInit 注册）；
//       GET/POST /config（设置页读写，storage 持久化）。
// 运行环境：宿主 QuickJS（es2020，全局 fetch 免权限）。不依赖 Node 内置模块。
import {
  createRouter,
  jsonResponse,
  parseQuery,
  createSearchHandler,
  createMusicUrlHandler,
} from '@songloft/plugin-sdk'

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
  if (j.code !== 200) throw new Error('kwqq ' + j.code + ': ' + (j.msg || ''))
  return j.data
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

    const per = await Promise.allSettled(cfg.sources.map(async (s) => {
      const data = await kwqq(cfg, '/' + s + '/search?keywords=' +
        encodeURIComponent(keyword) + '&page=' + (Number(page) || 1) + '&limit=' + limit, 4000)
      return (data.items || []).map(itemToResult)
    }))
    if (per[0] && per[0].status === 'rejected') {
      console.log('[kwqq] first source error: ' + String((per[0].reason && per[0].reason.message) || per[0].reason))
    }
    // 轮询交错合并（每源轮流出一个，保证多源在前列都有曝光），失败源静默跳过
    const lists = per.filter(r => r.status === 'fulfilled').map(r => r.value).filter(l => l.length)
    const merged = []
    for (let i = 0; i < limit; i++) {
      for (const l of lists) if (l[i]) merged.push(l[i])
    }
    const failed = per.filter(r => r.status === 'rejected').length
    console.log('[kwqq] search "' + keyword + '" p' + page + ' -> ' + merged.length +
      ' items (' + lists.length + '/' + cfg.sources.length + ' sources ok' +
      (failed ? ', ' + failed + ' failed' : '') + ')')
    if (!merged.length && failed === cfg.sources.length) failCache.set('search|' + cacheKey, true)
    searchCache.set(cacheKey, merged)
    return merged
  },
}))

// 播放直链：source_data {s,i,c} → kwqq /{s}/song/url?quality=…（320k 封顶）
router.post('/api/music/url', createMusicUrlHandler({
  resolveUrl: async (sd) => {
    if (!sd || !sd.s || !sd.i) throw new Error('source_data.s/i required')
    const cfg = await getConfig()
    let path = '/' + sd.s + '/song/url?id=' + encodeURIComponent(sd.i) + '&quality=' + (cfg.quality || '320k')
    if (sd.s === 'migu' && sd.c) path += '&copyright=' + encodeURIComponent(sd.c)
    const data = await kwqq(cfg, path, 10000)
    if (!data || !data.url) throw new Error('kwqq 未返回直链')
    const out = { url: data.url }
    if (data.headers && Object.keys(data.headers).length) out.headers = data.headers
    console.log('[kwqq] url ' + sd.s + '/' + sd.i + ' ext=' + data.ext + ' ' + data.bitrate_kbps + 'kbps')
    return out
  },
  // 主源失败换源：按宿主下发的 title/artist 自搜（kwqq 内部已按源顺序尝试）
  fallbackSearch: async (hint) => {
    if (!hint || !hint.title) return null
    const hit = await findHit(hint.title, hint.artist || '')
    return hit ? { source_data: hit.source_data, title: hit.title, artist: hit.artist } : null
  },
}))

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
