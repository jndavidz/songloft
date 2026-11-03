// kwqq /browse/* 在线浏览端点契约测试（spec: songloft .scratch/musicfree-style-library 票 06）
// 宿主桥与上游 kwqq-api 全部 stub，离线运行：node --test test/browse.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';

globalThis.songloft = {
  storage: {
    get: async () => null,
    set: async () => {},
  },
};

// —— 上游 stub：netease 透传（/netease/api/*，上游原样 JSON）、精装端点（/{source}/album）
// 与 kuwo 官方 wapi 免登通道（wapi.kuwo.cn / kbangserver / nplserver / mobileinterfaces）——
let neteaseResponses = {};
let premiumResponses = {};
let kuwoResponses = {};
let timeoutPaths = [];
const upstreamCalls = [];
globalThis.fetch = async (url) => {
  upstreamCalls.push(String(url));
  if (timeoutPaths.some((p) => url.includes(p))) {
    return new Promise((_, rej) => setTimeout(() => rej(new Error('kwqq timeout 8000ms')), 5));
  }
  const path = new URL(url).pathname + new URL(url).search;
  if (url.includes('wapi.kuwo.cn') || url.includes('kbangserver') || url.includes('nplserver') || url.includes('mobileinterfaces.kuwo.cn')) {
    // kuwo 官方通道按 URL 子串匹配（key 写完整可辨识片段）
    for (const key of Object.keys(kuwoResponses)) {
      if (url.includes(key)) {
        const v = kuwoResponses[key];
        if (v === 501) return { ok: false, status: 501, json: async () => ({}) };
        return { ok: true, status: 200, json: async () => v };
      }
    }
    return { ok: false, status: 404, json: async () => ({}) };
  }
  if (url.includes('/netease/api/')) {
    for (const key of Object.keys(neteaseResponses)) {
      if (path.startsWith('/netease/api/' + key)) {
        return { ok: true, status: 200, json: async () => neteaseResponses[key] };
      }
    }
    return { ok: false, status: 404, json: async () => ({}) };
  }
  for (const key of Object.keys(premiumResponses)) {
    if (path.startsWith('/' + key)) {
      const v = premiumResponses[key];
      if (v === 501) return { ok: false, status: 501, json: async () => ({ code: 501, msg: 'not implemented' }) };
      // kugou 透传 = 上游原样 JSON（现网容器实读形状，无 {code,msg,data} 信封）；其余精装带信封
      if (key.startsWith('kugou/api/')) return { ok: true, status: 200, json: async () => v };
      return { ok: true, status: 200, json: async () => ({ code: 200, msg: 'ok', data: v }) };
    }
  }
  // 精装单曲搜索专用兜底：/search/multi（P7）与 /kugou/api/* 透传不落这里
  if (url.includes('/search') && !url.includes('search/multi') && !url.includes('/kugou/api/')) {
    return {
      ok: true, status: 200,
      json: async () => ({
        code: 200, msg: 'ok',
        data: { items: [{ source: 'kuwo', id: 456, name: 'T', singer: 'A', album: 'AL', duration_s: 200, cover: 'c.jpg' }] },
      }),
    };
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
function setUpstream({ netease = {}, premium = {}, kuwo = {}, timeout = [] } = {}) {
  neteaseResponses = netease;
  premiumResponses = premium;
  kuwoResponses = kuwo;
  timeoutPaths = timeout;
}

const NETEASE_ALBUM = {
  code: 200,
  album: {
    id: 18918, name: 'Jay', picUrl: 'https://p/al.jpg', company: 'JM',
    publishTime: 973526400000, size: 10, description: 'desc',
    artist: { id: 6452, name: '周杰伦' },
  },
  songs: [
    { id: 186145, name: '可爱女人', dt: 239027, no: 1, al: { id: 18918, name: 'Jay', picUrl: 'https://p/al.jpg' }, ar: [{ id: 6452, name: '周杰伦' }] },
    { id: 186146, name: '完美主义', dt: 200000, no: 2, al: { id: 18918, name: 'Jay', picUrl: 'https://p/al.jpg' }, ar: [{ id: 6452, name: '周杰伦' }] },
  ],
};

test('browse/search：album 类型走 netease cloudsearch type=10，条目含 source 与分页', async () => {
  setUpstream({
    netease: {
      'cloudsearch?keywords=%E8%8C%83%E7%89%B9%E8%A5%BF&type=10': {
        code: 200,
        result: {
          albumCount: 1,
          albums: [{ id: 272566217, name: '范特西', picUrl: 'https://p/a.jpg', size: 1, publishTime: 1747756800000, artist: { name: 'YK' } }],
        },
      },
    },
  });
  const res = await post('/browse/search', { keywords: '范特西', type: 'album' });
  assert.equal(res.statusCode, 200);
  const out = JSON.parse(res.body);
  assert.equal(out.type, 'album');
  assert.equal(out.isEnd, true);
  assert.deepEqual(out.sources, ['netease']);
  assert.equal(out.items[0].id, '272566217');
  assert.equal(out.items[0].source, 'netease');
  assert.equal(out.items[0].track_count, 1);
  assert.ok(upstreamCalls[0].includes('type=10'));
});

test('browse/search：artist/sheet 类型映射 type=100/1000', async () => {
  setUpstream({
    netease: {
      'cloudsearch?keywords=x&type=100': { code: 200, result: { artistCount: 0, artists: [] } },
      'cloudsearch?keywords=x&type=1000': { code: 200, result: { playlistCount: 0, playlists: [] } },
    },
  });
  const r1 = await post('/browse/search', { keywords: 'x', type: 'artist' });
  const r2 = await post('/browse/search', { keywords: 'x', type: 'sheet' });
  assert.equal(JSON.parse(r1.body).type, 'artist');
  assert.equal(JSON.parse(r2.body).type, 'sheet');
  assert.ok(upstreamCalls.some((u) => u.includes('type=100')));
  assert.ok(upstreamCalls.some((u) => u.includes('type=1000')));
});

test('browse/search：music 类型多源聚合，条目带 dedup_key 与 source', async () => {
  setUpstream();
  const res = await post('/browse/search', { keywords: 'T', type: 'music' });
  assert.equal(res.statusCode, 200);
  const out = JSON.parse(res.body);
  assert.equal(out.type, 'music');
  assert.equal(out.items[0].dedup_key, 'kwqq_kuwo_456');
  assert.equal(out.items[0].source, 'kuwo');
  assert.equal(out.items[0].source_data.s, 'kuwo');
  assert.equal(out.items[0].title, 'T');
});

test('browse/search：缺 keywords 返回 400', async () => {
  const res = await post('/browse/search', { type: 'album' });
  assert.equal(res.statusCode, 400);
});

test('album/info：netease 带 id 走透传，曲序/公司/发行时间如实归一', async () => {
  setUpstream({ netease: { 'album?id=18918': NETEASE_ALBUM } });
  const res = await post('/album/info', { source: 'netease', id: '18918' });
  assert.equal(res.statusCode, 200);
  const out = JSON.parse(res.body);
  assert.equal(out.album.name, 'Jay');
  assert.equal(out.album.company, 'JM');
  assert.equal(out.album.publish_time, 973526400000);
  assert.equal(out.tracks.length, 2);
  assert.equal(out.tracks[0].track_no, 1);
  assert.equal(out.tracks[0].dedup_key, 'kwqq_netease_186145');
  assert.equal(out.tracks[0].duration, 239);
  assert.equal(out.tracks[0].source_data.s, 'netease');
});

test('album/info：非 netease 走精装二段式，候选归一', async () => {
  setUpstream({
    premium: {
      'kugou/album?name=': {
        source: 'kugou', name: 'X', artist: 'A',
        albums: [{ id: 9, name: 'X', artist: 'A', cover: 'https://p/x.jpg' }],
        items: [{ source: 'kugou', id: 77, name: 'S1', singer: 'A', album: 'X', duration_s: 180, cover: 'c.jpg', track_no: 1 }],
        total: 1,
      },
    },
  });
  const res = await post('/album/info', { source: 'kugou', name: 'X', artist: 'A' });
  assert.equal(res.statusCode, 200);
  const out = JSON.parse(res.body);
  assert.equal(out.album.source, 'kugou');
  assert.equal(out.album.id, '9');
  assert.equal(out.tracks[0].track_no, 1);
  assert.equal(out.tracks[0].dedup_key, 'kwqq_kugou_77');
});

test('album/info：上游 501 如实透出（kuwo 现已接入精装，上游不可用仍归一 501）', async () => {
  setUpstream({ premium: { 'kuwo/album?name=': 501 } });
  const res = await post('/album/info', { source: 'kuwo', name: 'X' });
  assert.equal(res.statusCode, 501);
  const out = JSON.parse(res.body);
  assert.equal(out.unsupported, true);
  assert.ok(out.error.length > 0);
});

test('album/info：bilibili 无精装通道 → 路由层 501 envelope', async () => {
  const res = await post('/album/info', { source: 'bilibili', name: 'X' });
  assert.equal(res.statusCode, 501);
  assert.equal(JSON.parse(res.body).unsupported, true);
});

test('album/info：缺 id 与 name → 400；不支持的源 → 501', async () => {
  const res = await post('/album/info', { source: 'kugou' });
  assert.equal(res.statusCode, 400);
  const res2 = await post('/album/info', { source: 'bilibili', name: 'X' });
  assert.equal(res2.statusCode, 501);
  assert.equal(JSON.parse(res2.body).unsupported, true);
});

test('album/info：上游超时 → 502', async () => {
  setUpstream({ timeout: ['kugou/album'] });
  const res = await post('/album/info', { source: 'kugou', name: 'X' });
  assert.equal(res.statusCode, 502);
});

test('artist/info + artist/works：详情/单曲/专辑三形状归一', async () => {
  setUpstream({
    netease: {
      'artists?id=6452': {
        code: 200,
        artist: { id: 6452, name: '周杰伦', picUrl: 'https://p/ar.jpg', briefDesc: '简介', albumSize: 45, musicSize: 591 },
      },
      'artist/songs?id=6452': {
        code: 200, total: 1, more: false,
        songs: [{ id: 298317, name: '屋顶', dt: 319039, al: { id: 29597, name: '有点野', picUrl: 'https://p/t.jpg' }, ar: [{ id: 9612, name: '温岚' }, { id: 6452, name: '周杰伦' }] }],
      },
      'artist/album?id=6452': {
        code: 200, hotAlbums: [{ id: 18918, name: 'Jay', picUrl: 'https://p/al.jpg', size: 10, artist: { name: '周杰伦' } }],
        artist: { albumSize: 45 },
      },
    },
  });
  const r1 = await post('/artist/info', { id: '6452' });
  const a1 = JSON.parse(r1.body);
  assert.equal(a1.artist.name, '周杰伦');
  assert.equal(a1.artist.music_count, 591);
  assert.equal(a1.artist.source, 'netease');

  const r2 = await post('/artist/works', { id: '6452', type: 'music' });
  const a2 = JSON.parse(r2.body);
  assert.equal(a2.isEnd, true);
  assert.equal(a2.items[0].dedup_key, 'kwqq_netease_298317');
  assert.equal(a2.items[0].artist, '温岚 / 周杰伦');

  const r3 = await post('/artist/works', { id: '6452', type: 'album' });
  const a3 = JSON.parse(r3.body);
  assert.equal(a3.items[0].id, '18918');
  assert.equal(a3.items[0].track_count, 10);
});

test('sheet/info：曲目分页 + 首页带元数据，长歌单 isEnd 正确', async () => {
  setUpstream({
    netease: {
      'playlist/track/all?id=1&limit=2&offset=0': {
        code: 200, total: 3,
        songs: [{ id: 287063, name: '我怀念的', dt: 289066, al: { id: 28520, name: '逆光', picUrl: 'https://p/1.jpg' }, ar: [{ id: 9272, name: '孙燕姿' }] }],
      },
      'playlist/track/all?id=1&limit=2&offset=2': {
        code: 200, total: 3,
        songs: [{ id: 287064, name: '第三首', dt: 200000, al: { id: 28520, name: '逆光', picUrl: 'https://p/1.jpg' }, ar: [{ id: 9272, name: '孙燕姿' }] }],
      },
      'playlist/detail?id=1': {
        code: 200,
        playlist: { id: 1, name: '华语经典', coverImgUrl: 'https://p/pl.jpg', trackCount: 3, playCount: 18272950, description: 'd', creator: { nickname: '请叫我快乐冠军' } },
      },
    },
  });
  const r1 = await post('/sheet/info', { id: '1', page: 1, limit: 2 });
  const a1 = JSON.parse(r1.body);
  assert.equal(a1.sheet.name, '华语经典');
  assert.equal(a1.sheet.creator, '请叫我快乐冠军');
  assert.equal(a1.isEnd, false);
  assert.equal(a1.total, 3);
  const r2 = await post('/sheet/info', { id: '1', page: 2, limit: 2 });
  const a2 = JSON.parse(r2.body);
  assert.equal(a2.isEnd, true);
  assert.equal(a2.sheet, undefined);
});

test('toplists + toplist/detail：榜单列表与详情归一', async () => {
  setUpstream({
    netease: {
      'toplist': {
        code: 200,
        list: [{ id: 19723756, name: '飙升榜', coverImgUrl: 'https://p/top.jpg', updateFrequency: '每天更新', trackCount: 100, description: 'd' }],
      },
      'playlist/track/all?id=19723756&limit=50&offset=0': {
        code: 200, total: 1,
        songs: [{ id: 186145, name: '可爱女人', dt: 239027, al: { id: 18918, name: 'Jay', picUrl: 'https://p/a.jpg' }, ar: [{ id: 6452, name: '周杰伦' }] }],
      },
    },
  });
  const r1 = await post('/toplists');
  const a1 = JSON.parse(r1.body);
  assert.equal(a1.items[0].id, '19723756');
  assert.equal(a1.items[0].update_frequency, '每天更新');
  const r2 = await post('/toplist/detail', { id: '19723756' });
  const a2 = JSON.parse(r2.body);
  assert.equal(a2.tracks[0].dedup_key, 'kwqq_netease_186145');
});

test('recommend/tags + recommend/sheets：标签来自平台（pinned+分组），分类拉歌单', async () => {
  setUpstream({
    netease: {
      'playlist/hot': { code: 200, tags: [{ name: '华语', hot: true }, { name: '流行', hot: true }] },
      'playlist/catlist': {
        code: 200,
        sub: [{ name: '轻音乐', category: '0' }, { name: '摇滚', category: '1' }],
        categories: { '0': '语种', '1': '风格' },
      },
      'top/playlist?cat=%E5%8D%8E%E8%AF%AD&limit=30&offset=0&order=hot': {
        code: 200, total: 1,
        playlists: [{ id: 5151027000, name: '仲夏夜之梦', coverImgUrl: 'https://p/c.jpg', trackCount: 40, playCount: 3050369, creator: { nickname: 'n' } }],
      },
    },
  });
  const r1 = await post('/recommend/tags');
  const a1 = JSON.parse(r1.body);
  assert.deepEqual(a1.pinned, [{ id: '华语', title: '华语' }, { id: '流行', title: '流行' }]);
  assert.equal(a1.categories.length, 2);
  const r2 = await post('/recommend/sheets', { cat: '华语' });
  const a2 = JSON.parse(r2.body);
  assert.equal(a2.items[0].id, '5151027000');
  assert.equal(a2.items[0].play_count, 3050369);
  assert.ok(upstreamCalls.some((u) => u.includes('cat=%E5%8D%8E%E8%AF%AD')));
});

test('music/comments：IComment 归一（热评优先/点赞数/毫秒时间戳/回复）', async () => {
  setUpstream({
    netease: {
      'comment/music?id=186145&limit=20&offset=0': {
        code: 200, total: 36808, more: true,
        hotComments: [{ commentId: 1, content: '热评', likedCount: 100, time: 1700000000000, user: { nickname: '甲', avatarUrl: 'https://p/u1.jpg' } }],
        comments: [{
          commentId: 2, content: '普通', likedCount: 3, time: 1700000001000,
          user: { nickname: '乙', avatarUrl: 'https://p/u2.jpg' },
          beReplied: [{ beRepliedCommentId: 9, content: '回复', user: { nickname: '丙' } }],
        }],
      },
    },
  });
  const res = await post('/music/comments', { id: '186145' });
  assert.equal(res.statusCode, 200);
  const out = JSON.parse(res.body);
  assert.equal(out.total, 36808);
  assert.equal(out.has_more, true);
  assert.equal(out.hot[0].like, 100);
  assert.equal(out.hot[0].createAt, 1700000000000);
  assert.equal(out.comments[0].replies[0].nickName, '丙');
  // 二次调用命中 commentCache（上游计数不变）
  const r2 = await post('/music/comments', { id: '186145' });
  assert.equal(JSON.parse(r2.body).total, 36808);
  assert.equal(upstreamCalls.filter((u) => u.includes('comment/music')).length, 1);
});

test('music/comments：缺 id 返回 400', async () => {
  const res = await post('/music/comments', {});
  assert.equal(res.statusCode, 400);
});

test('browse 端点：非法 JSON body 返回 400', async () => {
  const res = await post('/browse/search', undefined);
  // body undefined → {} → keywords required 400
  assert.equal(res.statusCode, 400);
  const res2 = await globalThis.onHTTPRequest({
    method: 'POST', path: '/browse/search', query: {},
    headers: { 'Content-Type': 'application/json' }, body: '{oops',
  });
  assert.equal(res2.statusCode, 400);
});

test('browse 端点：netease 上游超时 → 502（toplists）', async () => {
  setUpstream({ timeout: ['toplist'] });
  // 先经 /config 清空模块级缓存，避免命中前序用例缓存
  await post('/config', {});
  const res = await post('/toplists');
  assert.equal(res.statusCode, 502);
});

test('music/comments：无 id 时按 title+artist 回搜 netease id', async () => {
  setUpstream({
    netease: {
      'cloudsearch?keywords=T%20A&type=1&limit=1&offset=0': {
        code: 200, result: { songCount: 1, songs: [{ id: 186145, name: 'T' }] },
      },
      'comment/music?id=186145&limit=20&offset=0': {
        code: 200, total: 1, more: false, hotComments: [],
        comments: [{ commentId: 5, content: 'c', likedCount: 1, time: 1, user: { nickname: 'n' } }],
      },
    },
  });
  const res = await post('/music/comments', { title: 'T', artist: 'A' });
  assert.equal(res.statusCode, 200);
  const out = JSON.parse(res.body);
  assert.equal(out.comments[0].id, '5');
  assert.ok(upstreamCalls.some((u) => u.includes('type=1&limit=1')));
});

test('browse/search：类型搜索多源 fan-out（netease+kugou 透传，server multi 归一）', async () => {
  setUpstream({
    netease: {
      'cloudsearch?keywords=%E5%91%A8%E6%9D%B0%E4%BC%A6&type=10': {
        code: 200,
        result: { albumCount: 1, albums: [{ id: 1, name: 'N专辑', picUrl: 'https://p/n.jpg', size: 5, artist: { name: '周杰伦' } }] },
      },
    },
    premium: {
      'kugou/api/search?': {
        // 现网实读形状：透传原样 JSON，lists/total 嵌在 data 里
        status: 1, error_code: 0, error_msg: '',
        data: {
          page: 1, pagesize: 20, total: 1,
          lists: [{ albumid: 958706, albumname: 'K专辑', singer: '周杰伦', img: 'https://p/k.jpg' }],
        },
      },
      'qq/search/multi?': { items: [{ id: '8217', name: 'Q专辑', artist: '周杰伦', cover: 'https://p/q.jpg' }], total: 1 },
    },
  });
  const res = await post('/browse/search', { keywords: '周杰伦', type: 'album' });
  assert.equal(res.statusCode, 200);
  const out = JSON.parse(res.body);
  // 轮询交错：N、K、Q 各出一行
  assert.deepEqual(out.items.map(x => x.source), ['netease', 'kugou', 'qq']);
  assert.equal(out.items[1].id, '958706');
  assert.equal(out.items[1].name, 'K专辑');
  assert.equal(out.items[2].cover_url, 'https://p/q.jpg');   // server multi 的 cover → cover_url 归一
  assert.ok(upstreamCalls.some(u => u.includes('/qq/search/multi?') && u.includes('type=album')));
});

test('artist/info + works：kugou 透传通道（detail/audios/albums）', async () => {
  setUpstream({
    premium: {
      'kugou/api/artist/detail?': {
        status: 1, error_code: 0,
        data: { author_id: 3520, author_name: '周杰伦', sizable_avatar: 'http://img/{size}/a.jpg',
                album_count: 49, song_count: 1859, long_intro: [{ content: '简介' }] },
      },
      'kugou/api/artist/audios?': {
        total: 1,
        data: [{ album_audio_id: 1, audio_name: '歌曲A', author_name: '周杰伦', album_name: '专辑X',
                 hash_128: 'HASHA', timelength_128: 239000, trans_param: { union_cover: 'https://p/c.jpg' } }],
      },
      'kugou/api/artist/albums?': {
        total: 1,
        data: [{ album_id: 958706, album_name: '专辑B', sizable_cover: 'http://img/{size}/b.jpg', author_name: '周杰伦' }],
      },
    },
  });
  const r1 = await post('/artist/info', { id: '3520', source: 'kugou' });
  const a1 = JSON.parse(r1.body);
  assert.equal(a1.artist.name, '周杰伦');
  assert.equal(a1.artist.avatar_url, 'http://img/240/a.jpg');
  assert.equal(a1.artist.source, 'kugou');
  assert.equal(a1.artist.music_count, 1859);   // 现网字段是 song_count

  const r2 = await post('/artist/works', { id: '3520', type: 'music', source: 'kugou' });
  const a2 = JSON.parse(r2.body);
  assert.equal(a2.items[0].dedup_key, 'kwqq_kugou_HASHA');
  assert.equal(a2.items[0].duration, 239);

  const r3 = await post('/artist/works', { id: '3520', type: 'album', source: 'kugou' });
  const a3 = JSON.parse(r3.body);
  assert.equal(a3.items[0].id, '958706');
  assert.equal(a3.items[0].cover_url, 'http://img/240/b.jpg');
});

test('artist/info：不支持源（qq）→ 501 envelope', async () => {
  const res = await post('/artist/info', { id: '4558', source: 'qq' });
  assert.equal(res.statusCode, 501);
  assert.equal(JSON.parse(res.body).unsupported, true);
});

// ===== kugou 浏览多源（2026-11-04 批：榜单/推荐/歌单详情按 source 分发）=====
// 形状依据 2026-11-03 现网实测：rank/audio 用 audio_info，歌单 track/all 必须走
// global_collection_id（specialid 20010），tag/top playlist 走 data 信封。

test('toplists：kugou rank/list 与 netease 多源聚合，items 各带 source', async () => {
  await post('/config', {});
  setUpstream({
    netease: {
      'toplist': { code: 200, list: [{ id: 19723756, name: '飙升榜', coverImgUrl: 'https://p/top.jpg', updateFrequency: '每天更新' }] },
    },
    premium: {
      'kugou/api/rank/list': {
        data: { info: [{ rankid: 8888, rankname: 'TOP500', album_img_9: 'http://img/{size}/kg.jpg', update_frequency: '每天更新', intro: 'i' }] },
      },
    },
  });
  const res = await post('/toplists');
  assert.equal(res.statusCode, 200);
  const a = JSON.parse(res.body);
  assert.deepEqual(a.items.map(x => x.source), ['netease', 'kugou']);
  assert.equal(a.items[1].id, '8888');
  assert.equal(a.items[1].name, 'TOP500');
  assert.equal(a.items[1].cover_url, 'http://img/240/kg.jpg');  // {size} 占位归一
});

test('toplists：单源失败静默跳过（netease 挂，kugou 仍有结果）', async () => {
  await post('/config', {});
  setUpstream({ timeout: ['toplist'] });
  setUpstream({
    timeout: ['toplist'],
    premium: {
      'kugou/api/rank/list': { data: { info: [{ rankid: 8888, rankname: 'TOP500' }] } },
    },
  });
  const res = await post('/toplists');
  assert.equal(res.statusCode, 200);
  const a = JSON.parse(res.body);
  assert.deepEqual(a.items.map(x => x.source), ['kugou']);
});

test('toplist/detail：source=kugou 走 rank/audio，hash/dedup/source_data 归一', async () => {
  await post('/config', {});
  setUpstream({
    premium: {
      'kugou/api/rank/audio?': {
        total: 2, status: 1,
        data: {
          total: 2,
          songlist: [{
            songname: '时过境迁', album_audio_id: 967249675,
            authors: [{ author_name: '苏艺馨Chency' }],
            album_info: { album_name: '时过境迁', sizable_cover: 'http://img/{size}/c.jpg' },
            audio_info: { hash_128: 'H128', hash_320: 'H320', duration_128: 236000 },
          }],
        },
      },
    },
  });
  const res = await post('/toplist/detail', { id: '8888', source: 'kugou' });
  assert.equal(res.statusCode, 200);
  const a = JSON.parse(res.body);
  assert.equal(a.total, 2);
  assert.equal(a.isEnd, true);  // 1 * 50 >= 2
  const t = a.tracks[0];
  assert.equal(t.title, '时过境迁');
  assert.equal(t.source, 'kugou');
  assert.equal(t.dedup_key, 'kwqq_kugou_967249675');
  assert.equal(t.duration, 236);
  assert.equal(t.cover_url, 'http://img/240/c.jpg');
  assert.equal(t.source_data.i, 'H320');   // 播放 id 口径 = 320k hash 优先
  assert.equal(t.source_data.s, 'kugou');
  assert.ok(upstreamCalls.some(u => u.includes('/kugou/api/rank/audio?rankid=8888')));
});

test('sheet/info：source=kugou 走 playlist/track/all（gid 口径）+ list_info 元数据', async () => {
  await post('/config', {});
  setUpstream({
    premium: {
      'kugou/api/playlist/track/all?': {
        status: 1,
        data: {
          count: 99,
          list_info: {
            global_collection_id: 'collection_3_978504152_15_0', name: '经典华语金曲',
            list_create_username: 'swift', pic: 'http://img/{size}/p.jpg', intro: '简介', count: 99,
          },
          songs: [{
            name: '光良 - 童话', hash: 'H472', mixsongid: 32164767, timelen: 260000,
            albuminfo: { name: '童话' }, singerinfo: [{ name: '光良' }],
            cover: 'http://img/{size}/song.jpg',
            relate_goods: [{ hash: 'H320G', level: 4 }, { hash: 'H472', level: 2 }],
          }],
        },
      },
    },
  });
  const res = await post('/sheet/info', { id: 'collection_3_978504152_15_0', source: 'kugou' });
  assert.equal(res.statusCode, 200);
  const a = JSON.parse(res.body);
  assert.equal(a.total, 99);
  assert.equal(a.isEnd, false);  // 1 * 50 < 99
  assert.equal(a.sheet.name, '经典华语金曲');
  assert.equal(a.sheet.creator, 'swift');
  assert.equal(a.sheet.cover_url, 'http://img/480/p.jpg');
  const t = a.tracks[0];
  assert.equal(t.title, '童话');         // 「歌手 - 歌名」拆分
  assert.equal(t.artist, '光良');
  assert.equal(t.source_data.i, 'H320G'); // 320k hash 优先（relate_goods level 4）
  assert.equal(t.dedup_key, 'kwqq_kugou_32164767');
});

test('recommend/tags：多源 platforms（netease 顶层兼容 + kugou 分组）', async () => {
  await post('/config', {});
  setUpstream({
    netease: {
      'playlist/hot': { code: 200, tags: [{ name: '华语', hot: true }] },
      'playlist/catlist': { code: 200, sub: [{ name: '轻音乐', category: '0' }], categories: { '0': '语种' } },
    },
    premium: {
      'kugou/api/playlist/tags': {
        status: 1,
        data: [
          { tag_name: '热歌', son: [{ tag_id: 1, tag_name: '热歌' }, { tag_id: 2, tag_name: '新歌' }] },
          { tag_name: '语种', son: [{ tag_id: 3, tag_name: '国语' }] },
        ],
      },
    },
  });
  const res = await post('/recommend/tags');
  assert.equal(res.statusCode, 200);
  const a = JSON.parse(res.body);
  assert.equal(a.pinned.length, 1);  // 顶层兼容字段仍是 netease
  assert.equal(a.platforms.length, 2);
  assert.deepEqual(a.platforms.map(p => p.source), ['netease', 'kugou']);
  assert.equal(a.platforms[1].pinned[0].id, '1');
  assert.equal(a.platforms[1].categories.length, 2);
  assert.equal(a.platforms[1].categories[0].title, '热歌');
});

test('recommend/sheets：source=kugou 走 top/playlist，gid 口径 + has_next 驱动 isEnd', async () => {
  await post('/config', {});
  setUpstream({
    premium: {
      'kugou/api/top/playlist?': {
        status: 1,
        data: {
          has_next: 1,
          special_list: [{
            specialid: 8743644, global_collection_id: 'collection_3_978504152_15_0',
            specialname: '经典华语金曲', nickname: 'swift',
            flexible_cover: 'http://c1/{size}/c.jpg', play_count: 49059412,
          }],
        },
      },
    },
  });
  const res = await post('/recommend/sheets', { cat: '1', source: 'kugou' });
  assert.equal(res.statusCode, 200);
  const a = JSON.parse(res.body);
  assert.equal(a.isEnd, false);
  assert.equal(a.items[0].id, 'collection_3_978504152_15_0');  // gid 优先（详情可达）
  assert.equal(a.items[0].source, 'kugou');
  assert.equal(a.items[0].play_count, 49059412);
  assert.ok(upstreamCalls.some(u => u.includes('category_id=1')));
});

// ===== kuwo 官方 wapi 免登通道（2026-11-04 批：榜单/推荐/歌单详情直连）=====
// 形状依据 2026-11-04 现网实测：bang/list 分组 child、ksong.s musiclist、
// getTagList digest 语义、getRcmPlayList 信封、pl.svc 元数据与 rid 口径。

test('toplists：kuwo bang/list 平铺为 items（三源聚合）', async () => {
  await post('/config', {});
  setUpstream({
    netease: { 'toplist': { code: 200, list: [{ id: 19723756, name: '飙升榜' }] } },
    premium: { 'kugou/api/rank/list': { data: { info: [{ rankid: 8888, rankname: 'TOP500' }] } } },
    kuwo: {
      'api/pc/bang/list': {
        child: [
          { disname: '官方榜', child: [{ sourceid: 16, name: '酷我热歌榜', pic5: 'https://p/kw.png', intro: 'i' }] },
        ],
      },
    },
  });
  const res = await post('/toplists');
  assert.equal(res.statusCode, 200);
  const a = JSON.parse(res.body);
  assert.deepEqual(a.items.map(x => x.source), ['netease', 'kugou', 'kuwo']);
  const kw = a.items[2];
  assert.equal(kw.id, '16');
  assert.equal(kw.name, '酷我热歌榜');
  assert.equal(kw.cover_url, 'https://p/kw.png');
});

test('toplist/detail：source=kuwo 走 ksong.s，rid 口径归一', async () => {
  await post('/config', {});
  setUpstream({
    kuwo: {
      'ksong.s': {
        num: 300,
        musiclist: [{ id: 624683929, name: '山风山风等等我', artist: '万海东', album: '山风山风等等我', song_duration: 245 }],
      },
    },
  });
  const res = await post('/toplist/detail', { id: '16', source: 'kuwo' });
  assert.equal(res.statusCode, 200);
  const a = JSON.parse(res.body);
  assert.equal(a.isEnd, true); // 1 首不足 50 → 尽
  const t = a.tracks[0];
  assert.equal(t.source, 'kuwo');
  assert.equal(t.dedup_key, 'kwqq_kuwo_624683929');
  assert.equal(t.duration, 245);
  assert.equal(t.source_data.i, '624683929');
  assert.equal(t.source_data.s, 'kuwo');
});

test('sheet/info：source=kuwo 走 pl.svc，rid 口径 + 元数据', async () => {
  await post('/config', {});
  setUpstream({
    kuwo: {
      'pl.svc': {
        id: 2399331172, title: '经典｜用粤语歌唤起年少的记忆', uname: 'Boom',
        pic: 'http://img1/c.jpg', playnum: 506702, info: '简介', total: 24,
        musiclist: [{ id: 80458, name: '沉默是金', artist: '张国荣', album: '粤语超白金', duration: 252, albumpic: 'http://img1/al.jpg' }],
      },
    },
  });
  const res = await post('/sheet/info', { id: '2399331172', source: 'kuwo' });
  assert.equal(res.statusCode, 200);
  const a = JSON.parse(res.body);
  assert.equal(a.total, 24);
  assert.equal(a.isEnd, false); // 1 首 < 24，未到末页
  assert.equal(a.sheet.name, '经典｜用粤语歌唤起年少的记忆');
  assert.equal(a.sheet.creator, 'Boom');
  assert.equal(a.sheet.play_count, 506702);
  const t = a.tracks[0];
  assert.equal(t.title, '沉默是金');
  assert.equal(t.source_data.i, '80458');
  assert.equal(t.cover_url, 'http://img1/al.jpg');
});

test('recommend/tags：kuwo getTagList 分组 + digest 编码（专区 id 带 @）', async () => {
  await post('/config', {});
  setUpstream({
    netease: { 'playlist/hot': { code: 200, tags: [{ name: '华语' }] } },
    kuwo: {
      'getTagList': {
        data: [
          { name: '专区', data: [{ id: 211, name: '经典老歌专区', digest: '43' }] },
          { name: '主题', data: [{ id: 999, name: '热歌', digest: '10000' }] },
        ],
      },
    },
  });
  const res = await post('/recommend/tags');
  assert.equal(res.statusCode, 200);
  const a = JSON.parse(res.body);
  const kwP = a.platforms.find(p => p.source === 'kuwo');
  assert.ok(kwP, 'kuwo platform 缺失');
  const qzTag = kwP.categories[0].data[0];
  assert.equal(qzTag.id, '211@43');
  const hotTag = kwP.categories[1].data[0];
  assert.equal(hotTag.id, '999');
  assert.deepEqual(kwP.pinned, [{ id: '999', title: '热歌' }]);
});

test('recommend/sheets：source=kuwo 空标签走 getRcmPlayList，专区走 er.s 切片', async () => {
  await post('/config', {});
  setUpstream({
    kuwo: {
      'getRcmPlayList': {
        data: {
          total: 1753,
          data: [{ id: 2399331172, name: '经典｜用粤语歌唤起年少的记忆', uname: 'Boom', img: 'http://img1/c.jpg', total: '24', listencnt: '506702' }],
        },
      },
      'er.s': [
        { name: '经典老歌专区', list: [{ id: 3003724795, name: '经典国语', img: 'http://img1/e.jpg' }, { id: 3003724796, name: '粤语' }] },
      ],
    },
  });
  // 空标签（全部推荐）
  const r1 = await post('/recommend/sheets', { cat: '', source: 'kuwo' });
  assert.equal(r1.statusCode, 200);
  const a1 = JSON.parse(r1.body);
  assert.equal(a1.isEnd, false); // 1*30 < 1753
  assert.equal(a1.items[0].id, '2399331172');
  assert.equal(a1.items[0].source, 'kuwo');
  assert.equal(a1.items[0].play_count, 506702);
  // 专区标签（@digest）
  const r2 = await post('/recommend/sheets', { cat: '211@43', source: 'kuwo', limit: 1 });
  const a2 = JSON.parse(r2.body);
  assert.equal(a2.items.length, 1);
  assert.equal(a2.items[0].id, '3003724795');
  assert.equal(a2.isEnd, false); // 1*1 < 2
  assert.ok(upstreamCalls.some(u => u.includes('get_pc_qz_data')));
});
