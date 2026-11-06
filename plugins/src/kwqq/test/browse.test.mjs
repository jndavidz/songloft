// kwqq /browse/* 在线浏览端点契约测试（2026-11-04 P9 全门面化版）
// 宿主桥与上游 kwqq-api 全部 stub，离线运行：node --test test/browse.test.mjs
// P9 后插件不再直连任何透传路径（/netease/api/*、/kugou/api/* 已收编进统一门面），
// 测试桩退化为纯信封模式：全部门面响应 = {code:200, msg:'ok', data:…}。

import { test } from 'node:test';
import assert from 'node:assert/strict';

globalThis.songloft = {
  storage: {
    get: async () => null,
    set: async () => {},
  },
};

// —— 上游 stub：kwqq-api 门面响应统一走 {code,msg,data} 信封（kwqq() 拆包）——
let premiumResponses = {};
let timeoutPaths = [];
const upstreamCalls = [];
globalThis.fetch = async (url) => {
  upstreamCalls.push(String(url));
  if (timeoutPaths.some((p) => url.includes(p))) {
    return new Promise((_, rej) => setTimeout(() => rej(new Error('kwqq timeout 8000ms')), 5));
  }
  const path = new URL(url).pathname + new URL(url).search;
  for (const key of Object.keys(premiumResponses)) {
    if (path.startsWith('/' + key)) {
      const v = premiumResponses[key];
      if (v === 501) return { ok: false, status: 501, json: async () => ({ code: 501, msg: 'not implemented' }) };
      return { ok: true, status: 200, json: async () => ({ code: 200, msg: 'ok', data: v }) };
    }
  }
  return { ok: false, status: 404, json: async () => ({}) };
};

await import('../src/main.js');

function post(path, body) {
  return globalThis.onHTTPRequest({
    method: 'POST',
    path,
    query: {},
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

// 每个用例独立上游配置
function setUpstream({ premium = {}, timeout = [] } = {}) {
  premiumResponses = premium;
  timeoutPaths = timeout;
}

// —— 常用归一形状样例（server 门面契约）——
const NE_SEARCH_ITEM = { id: '186145', name: '可爱女人', singer: '周杰伦', album: 'Jay', duration_s: 239, cover: 'https://p/n.jpg', source: 'netease' };

test('browse/search：album 类型走门面 search/multi，多源轮询交错（kugou/netease/qq/kuwo）', async () => {
  await post('/config', {});
  setUpstream({
    premium: {
      'netease/search/multi?': { source: 'netease', type: 'album', total: 1, items: [{ id: '1', name: 'N专辑', artist: '周杰伦', cover: 'https://p/n.jpg', track_count: 5, source: 'netease' }] },
      'kugou/search/multi?': { source: 'kugou', type: 'album', total: 1, items: [{ id: '958706', name: 'K专辑', artist: '周杰伦', cover: 'https://p/k.jpg', track_count: 3, source: 'kugou' }] },
      'qq/search/multi?': { items: [{ id: '8217', name: 'Q专辑', artist: '周杰伦', cover: 'https://p/q.jpg', source: 'qq' }], total: 1 },
      'kuwo/search/multi?': { items: [], total: 0 },
      'migu/search/multi?': 501,
      'qianqian/search/multi?': 501,
    },
  });
  const res = await post('/browse/search', { keywords: '周杰伦', type: 'album' });
  assert.equal(res.statusCode, 200);
  const out = JSON.parse(res.body);
  // 轮询交错：kugou/netease/qq 各出一行（kuwo 空、migu/qianqian 501 跳过）；顺序以 SEARCH_SOURCE_ORDER 为准（酷狗在前）
  assert.deepEqual(out.items.map(x => x.source), ['kugou', 'netease', 'qq']);
  assert.equal(out.items[0].cover_url, 'https://p/k.jpg');   // 门面 cover → 契约 cover_url
  assert.equal(out.sources.length, 3);
  assert.ok(upstreamCalls.some(u => u.includes('/netease/search/multi?') && u.includes('type=album')));
  assert.ok(upstreamCalls.some(u => u.includes('/kugou/search/multi?')));
});

test('browse/search：music 类型多源聚合精装 /{source}/search（不变路径）', async () => {
  await post('/config', {});
  setUpstream({
    premium: {
      'netease/search?': { total: 1, items: [{ source: 'netease', id: 1, name: 'N', singer: 'A', album: 'AL', duration_s: 200 }] },
      'kugou/search?': { total: 1, items: [{ source: 'kugou', id: 'K1', name: 'K', singer: 'A', album: 'AL', duration_s: 190 }] },
      'kuwo/search?': { total: 0, items: [] },
      'qq/search?': { total: 0, items: [] },
      'migu/search?': { total: 0, items: [] },
      'qianqian/search?': { total: 0, items: [] },
    },
  });
  const res = await post('/browse/search', { keywords: 'test', type: 'music' });
  assert.equal(res.statusCode, 200);
  const out = JSON.parse(res.body);
  // 单曲同样按 SEARCH_SOURCE_ORDER 交错：酷狗在前，其次网易云
  assert.deepEqual(out.items.map(x => x.source), ['kugou', 'netease']);
  assert.equal(out.items[1].dedup_key, 'kwqq_netease_1');
});

test('browse/search：缺 keywords 返回 400', async () => {
  const res = await post('/browse/search', { keywords: '' });
  assert.equal(res.statusCode, 400);
});

test('album/info：netease id 走 P9b /album/detail，extra 元数据 + track_no 补齐', async () => {
  await post('/config', {});
  setUpstream({
    premium: {
      'netease/album/detail?': {
        albums: [{ id: '18918', name: 'Jay', artist: '周杰伦', cover: 'https://p/al.jpg', size: 10,
                  extra: { publish_time: 973526400000, company: 'JM', description: 'desc' }, source: 'netease' }],
        items: [
          { id: '186145', name: '可爱女人', singer: '周杰伦', album: 'Jay', duration_s: 239, source: 'netease' },
          { id: '186146', name: '完美主义', singer: '周杰伦', album: 'Jay', duration_s: 200, source: 'netease' },
        ],
        total: 2,
      },
    },
  });
  const res = await post('/album/info', { id: '18918', source: 'netease' });
  assert.equal(res.statusCode, 200);
  const a = JSON.parse(res.body);
  assert.equal(a.album.name, 'Jay');
  assert.equal(a.album.company, 'JM');
  assert.equal(a.album.publish_time, 973526400000);
  assert.equal(a.tracks[0].dedup_key, 'kwqq_netease_186145');
  assert.equal(a.tracks[0].track_no, 1);
  assert.equal(a.tracks[1].track_no, 2);
  assert.ok(upstreamCalls.some(u => u.includes('/netease/album/detail?id=18918')));
});

test('album/info：非 netease 无 id 走精装二段式 /{source}/album?name=&artist=', async () => {
  await post('/config', {});
  setUpstream({
    premium: {
      'migu/album?': { source: 'migu', name: '测试专辑', artist: '歌手A', albums: [{ id: 'm1', name: '测试专辑', cover: 'c.jpg' }], items: [{ source: 'migu', id: 'm2', name: 'S', singer: 'A', album: '测试专辑', duration_s: 180 }], total: 1 },
    },
  });
  const res = await post('/album/info', { name: '测试专辑', artist: '歌手A', source: 'migu' });
  assert.equal(res.statusCode, 200);
  const a = JSON.parse(res.body);
  assert.equal(a.album.name, '测试专辑');
  assert.equal(a.tracks[0].dedup_key, 'kwqq_migu_m2');
});

test('album/info：bilibili 无精装通道 → 501 envelope', async () => {
  const res = await post('/album/info', { name: 'x', source: 'bilibili' });
  assert.equal(res.statusCode, 501);
  assert.equal(JSON.parse(res.body).unsupported, true);
});

test('artist/info + artist/works：P9 门面双源（music→itemToTrack，album→entryToAlbum）', async () => {
  await post('/config', {});
  setUpstream({
    premium: {
      'netease/artist?': { artist: { id: '6452', name: '周杰伦', avatar: 'http://a.jpg', alias: 'Jay', description: 'd', album_count: 49, music_count: 1859, source: 'netease' } },
      'netease/artist/works?': { type: 'music', page: 1, is_end: true, total: 1,
        items: [{ id: '186145', name: '可爱女人', singer: '周杰伦', album: 'Jay', duration_s: 239, source: 'netease' }] },
      'kugou/artist?': { artist: { id: '3520', name: '周杰伦', avatar: 'http://a2.jpg', album_count: 49, music_count: 1859, source: 'kugou' } },
      'kugou/artist/works?': { type: 'album', page: 1, is_end: true, total: 1,
        items: [{ id: '958706', name: '专辑B', artist: '周杰伦', cover: 'http://b.jpg', track_count: 10,
                  extra: { publish_time: '2020-03-01' }, source: 'kugou' }] },
      'qq/artist?': { artist: { id: '4558', name: '周杰伦', avatar: 'http://q.jpg', album_count: 43, music_count: 1013, source: 'qq' } },
    },
  });
  const r1 = await post('/artist/info', { id: '6452', source: 'netease' });
  const a1 = JSON.parse(r1.body);
  assert.equal(a1.artist.alias, 'Jay');
  assert.equal(a1.artist.avatar_url, 'http://a.jpg');

  const r2 = await post('/artist/works', { id: '6452', type: 'music', source: 'netease' });
  const a2 = JSON.parse(r2.body);
  assert.equal(a2.items[0].dedup_key, 'kwqq_netease_186145');

  const r3 = await post('/artist/info', { id: '3520', source: 'kugou' });
  assert.equal(JSON.parse(r3.body).artist.avatar_url, 'http://a2.jpg');

  const r4 = await post('/artist/works', { id: '3520', type: 'album', source: 'kugou' });
  const a4 = JSON.parse(r4.body);
  assert.equal(a4.items[0].id, '958706');
  assert.equal(a4.items[0].cover_url, 'http://b.jpg');
  // 上游 publish_time 是 'YYYY-MM-DD' 字符串，必须归一成毫秒数：客户端 `as num?` 才不抛。
  assert.equal(a4.items[0].publish_time, Date.parse('2020-03-01'));

  const r5 = await post('/artist/info', { id: '4558', source: 'qq' });
  assert.equal(r5.statusCode, 200);
  assert.equal(JSON.parse(r5.body).artist.name, '周杰伦');
});

test('sheet/info：netease/kugou/kuwo 三源门面（meta→sheet，extra.play_count 提取）', async () => {
  await post('/config', {});
  setUpstream({
    premium: {
      'netease/playlist/songs?': { source: 'netease', id: '5311363628', page: 1, is_end: false, total: 99,
        items: [NE_SEARCH_ITEM],
        meta: { id: '5311363628', name: 'N歌单', creator: 'u', cover: 'http://p/c.jpg', track_count: 99,
                play_count: 12345, extra: { description: '简介' } } },
      'kuwo/playlist/songs?': { source: 'kuwo', id: '2399331172', page: 1, is_end: false, total: 24,
        items: [{ id: '80458', name: '沉默是金', singer: '张国荣', album: '粤语超白金', duration_s: 252, source: 'kuwo' }],
        meta: { id: '2399331172', name: 'K歌单', creator: 'swift', cover: 'http://p/k.jpg', track_count: 24,
                play_count: 506702, extra: {} } },
    },
  });
  const r1 = await post('/sheet/info', { id: '5311363628', source: 'netease' });
  const a1 = JSON.parse(r1.body);
  assert.equal(a1.sheet.name, 'N歌单');
  assert.equal(a1.sheet.play_count, 12345);
  assert.equal(a1.tracks[0].dedup_key, 'kwqq_netease_186145');

  const r2 = await post('/sheet/info', { id: '2399331172', source: 'kuwo' });
  const a2 = JSON.parse(r2.body);
  assert.equal(a2.sheet.play_count, 506702);
  assert.equal(a2.tracks[0].source_data.i, '80458');

  const r3 = await post('/sheet/info', { id: 'x', source: 'migu' });
  assert.equal(r3.statusCode, 501);
});

test('toplists + toplist/detail：P8 门面三源聚合与分发', async () => {
  await post('/config', {});
  setUpstream({
    premium: {
      'netease/toplists': { source: 'netease', items: [{ id: '19723756', name: '飙升榜', cover: 'https://p/top.jpg', update_frequency: '每天更新', source: 'netease' }] },
      'kugou/toplists': { source: 'kugou', items: [{ id: '8888', name: 'TOP500', cover: 'http://img/240/kg.jpg', source: 'kugou' }] },
      'kuwo/toplists': { source: 'kuwo', items: [{ id: '16', name: '酷我热歌榜', cover: 'https://p/kw.png', source: 'kuwo' }] },
      'kuwo/toplist/songs?': { source: 'kuwo', id: '16', page: 1, is_end: false, total: 300,
        items: [{ id: '624683929', name: '山风山风等等我', singer: '万海东', album: '山风山风等等我', duration_s: 245, source: 'kuwo' }] },
    },
  });
  const r1 = await post('/toplists');
  const a1 = JSON.parse(r1.body);
  assert.deepEqual(a1.items.map(x => x.source), ['netease', 'kugou', 'kuwo']);
  assert.equal(a1.items[2].cover_url, 'https://p/kw.png');

  const r2 = await post('/toplist/detail', { id: '16', source: 'kuwo' });
  const a2 = JSON.parse(r2.body);
  assert.equal(a2.total, 300);
  assert.equal(a2.tracks[0].dedup_key, 'kwqq_kuwo_624683929');
  assert.equal(a2.isEnd, false);
});

test('recommend/tags + recommend/sheets：P8 门面三平台（顶层兼容 = netease 优先）', async () => {
  await post('/config', {});
  setUpstream({
    premium: {
      'netease/playlist/tags': { pinned: [{ id: '华语', title: '华语' }], groups: [{ title: '语种', data: [{ id: '华语', title: '华语' }] }] },
      'kugou/playlist/tags': { pinned: [{ id: '1', title: '热歌' }], groups: [{ title: '热歌', data: [{ id: '1', title: '热歌' }] }] },
      'kuwo/playlist/tags': { pinned: [{ id: '2189', title: '短视频' }], groups: [{ title: '专区', data: [{ id: '211@43', title: '经典老歌专区' }] }] },
      'kuwo/playlist/sheets?': { source: 'kuwo', tag: '211@43', page: 1, is_end: false, total: 1753,
        items: [{ id: '2399331172', name: '经典华语', creator: 'swift', cover: 'http://c1/c.jpg', track_count: 24, play_count: 506702, source: 'kuwo' }] },
    },
  });
  const r1 = await post('/recommend/tags');
  const a1 = JSON.parse(r1.body);
  assert.equal(a1.pinned[0].title, '华语');          // 顶层兼容字段 = netease
  assert.equal(a1.platforms.length, 3);
  assert.equal(a1.platforms[2].categories[0].data[0].id, '211@43');

  const r2 = await post('/recommend/sheets', { cat: '211@43', source: 'kuwo' });
  const a2 = JSON.parse(r2.body);
  assert.equal(a2.isEnd, false);
  assert.equal(a2.items[0].id, '2399331172');
  assert.equal(a2.items[0].play_count, 506702);
});

test('music/comments：P9 门面 /netease/comments（id 与 title 回搜两形态）', async () => {
  await post('/config', {});
  setUpstream({
    premium: {
      'netease/comments?': {
        id: '186145', page: 1, total: 36808, has_more: true,
        hot: [{ id: 'h1', nick_name: '用户A', avatar: 'http://a.jpg', content: '热评', like: 100, created_at: 1700000000000, replies: [] }],
        items: [{ id: 'c1', nick_name: '用户B', avatar: null, content: '评论', like: 1, created_at: 1700000000001, replies: [] }],
      },
    },
  });
  const r1 = await post('/music/comments', { id: '186145' });
  const a1 = JSON.parse(r1.body);
  assert.equal(a1.total, 36808);
  assert.equal(a1.hot[0].nickName, '用户A');      // IComment 字段名（nickName/comment）
  assert.equal(a1.hot[0].comment, '热评');
  assert.equal(a1.comments[0].createAt, 1700000000001);

  const r2 = await post('/music/comments', { title: '可爱女人', artist: '周杰伦' });
  const a2 = JSON.parse(r2.body);
  assert.equal(a2.total, 36808);
  assert.ok(upstreamCalls.some(u => u.includes('title=%E5%8F%AF%E7%88%B1%E5%A5%B3%E4%BA%BA')));
});

test('browse 端点：非法 JSON body 返回 400；上游超时 → 502', async () => {
  setUpstream({});
  const res2 = await globalThis.onHTTPRequest({
    method: 'POST', path: '/browse/search', query: {},
    headers: { 'Content-Type': 'application/json' }, body: '{oops',
  });
  assert.equal(res2.statusCode, 400);

  await post('/config', {});
  setUpstream({ timeout: ['netease/toplists'] });
  const res = await post('/toplists');
  assert.equal(res.statusCode, 502);
});
