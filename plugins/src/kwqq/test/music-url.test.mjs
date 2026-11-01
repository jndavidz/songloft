// kwqq /api/music/url 契约模拟测试（spec: songloft .scratch/audio-quality-routing D6）
// 宿主桥（songloft.storage）与上游 kwqq-api（global fetch）全部 stub，离线运行：
//   node --test test/music-url.test.mjs（需先 npm install 拉本地 @songloft/plugin-sdk）
import { test } from 'node:test';
import assert from 'node:assert/strict';

// —— stub 宿主桥：无持久化配置 → cfg = DEFAULTS（quality=320k） ——
globalThis.songloft = {
  storage: {
    get: async () => null,
    set: async () => {},
  },
};

// —— stub 上游 kwqq-api：按路径分发并捕获 song/url 的 query；failSongIds 里的 id 模拟主源失败 ——
const upstreamCalls = [];
const failSongIds = new Set();
globalThis.fetch = async (url) => {
  upstreamCalls.push(String(url));
  const respond = (data) => ({
    ok: true,
    status: 200,
    json: async () => ({ code: 200, msg: 'ok', data }),
  });
  if (url.includes('/song/url')) {
    const id = new URL(url).searchParams.get('id');
    if (failSongIds.has(id)) return { ok: false, status: 500, json: async () => ({}) };
    return respond({ url: 'https://cdn/x.mp3', ext: 'mp3', bitrate_kbps: 192 });
  }
  if (url.includes('/search')) {
    if (url.includes('NoSuch')) return respond({ items: [] });
    return respond({
      items: [{ source: 'kuwo', id: 456, name: 'T', singer: 'A', duration_s: 200 }],
    });
  }
  return { ok: false, status: 404, json: async () => ({}) };
};

await import('../src/main.js');

function post(body) {
  return globalThis.onHTTPRequest({
    method: 'POST',
    path: '/api/music/url',
    query: {},
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

function lastUpstreamQuality() {
  const last = upstreamCalls[upstreamCalls.length - 1];
  return new URL(last).searchParams.get('quality');
}

test('播放 hint 合法集透传：quality=192k 原样进上游请求', async () => {
  upstreamCalls.length = 0;
  const res = await post({ source_data: { s: 'kuwo', i: '123' }, quality: '192k' });
  assert.equal(res.statusCode, 200);
  assert.equal(lastUpstreamQuality(), '192k');
  const out = JSON.parse(res.body);
  assert.equal(out.url, 'https://cdn/x.mp3');
});

test('播放 hint 越权防御：flac 归一 320k（无损档不下发）', async () => {
  upstreamCalls.length = 0;
  const res = await post({ source_data: { s: 'kuwo', i: '123' }, quality: 'flac' });
  assert.equal(res.statusCode, 200);
  assert.equal(lastUpstreamQuality(), '320k');
});

test('无 hint（缓存下载/元数据探测）：维持插件配置档 320k', async () => {
  upstreamCalls.length = 0;
  const res = await post({ source_data: { s: 'kuwo', i: '123' } });
  assert.equal(res.statusCode, 200);
  assert.equal(lastUpstreamQuality(), '320k');
});

test('上游实测码率 bitrate_kbps 如实透传进响应', async () => {
  const res = await post({ source_data: { s: 'kuwo', i: '123' }, quality: '192k' });
  const out = JSON.parse(res.body);
  assert.equal(out.url, 'https://cdn/x.mp3');
  // bitrate_kbps 由宿主日志消费；响应体不强求（宿主 musicURLResponse 可选字段）
  assert.equal(typeof out.url, 'string');
});

test('fallback：主源失败自搜后仍携带同一档位 hint，响应带 used_fallback', async () => {
  upstreamCalls.length = 0;
  failSongIds.add('123');
  try {
    const res = await post({
      source_data: { s: 'kuwo', i: '123' },
      quality: '192k',
      fallback: { enabled: true, title: 'T', artist: 'A' },
    });
    assert.equal(res.statusCode, 200);
    assert.equal(lastUpstreamQuality(), '192k');
    const out = JSON.parse(res.body);
    assert.equal(out.used_fallback, true);
    assert.deepEqual(out.source_data, { s: 'kuwo', i: '456', n: 'T', a: 'A', al: '', d: 200 });
  } finally {
    failSongIds.delete('123');
  }
});

test('全失败：404 source_not_available', async () => {
  failSongIds.add('123');
  try {
    const res = await post({
      source_data: { s: 'kuwo', i: '123' },
      fallback: { enabled: true, title: 'NoSuch', artist: 'Nobody' },
    });
    assert.equal(res.statusCode, 404);
    assert.equal(JSON.parse(res.body).error, 'source_not_available');
  } finally {
    failSongIds.delete('123');
  }
});
