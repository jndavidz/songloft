// kwqq /cover-search 精确取封面测试（§2.77-50：按 rawID 取封面方案）
// 宿主传 source+raw_id 时按 /{source}/song/info?id={rawID} 取 cover；失败回退标题搜索。
import { test } from 'node:test';
import assert from 'node:assert/strict';

// 宿主桥 stub
globalThis.songloft = {
  storage: { get: async () => null, set: async () => {} },
};

// 上游 stub：记录请求路径，按路径返回信封数据
let upstreamCalls = [];
let songInfoResp = { cover: '' };  // 可配：{cover, name, singer}
let searchResp = { items: [] };
globalThis.fetch = async (url) => {
  const u = String(url);
  upstreamCalls.push(u);
  const respond = (data) => ({ ok: true, status: 200,
    json: async () => ({ code: 200, msg: 'ok', data }) });
  if (u.includes('/song/info')) return respond(songInfoResp);
  if (u.includes('/search')) return respond(searchResp);
  return { ok: false, status: 404, json: async () => ({}) };
};

await import('../src/main.js');

function get(path, queryStr) {
  return globalThis.onHTTPRequest({
    method: 'GET', path,
    query: queryStr,           // parseQuery 要字符串，不是对象
    headers: {},
  });
}

test('cover-search：有 source+raw_id 时按 /{source}/song/info 精确取 cover', async () => {
  upstreamCalls = [];
  songInfoResp = { name: '晴天', singer: '周杰伦', cover: 'http://img.kugou/qt.jpg' };
  const res = await get('/cover-search',
    'title=%E6%99%B4%E5%A4%A9&source=kugou&raw_id=B3A52A7A958BF0AED0EBFBA2E9A818B7');
  assert.equal(res.statusCode, 200);
  const out = JSON.parse(res.body);
  assert.equal(out.cover_url, 'http://img.kugou/qt.jpg');
  assert.ok(upstreamCalls.some(u => u.includes('/kugou/song/info?id=B3A52A7A958BF0AED0EBFBA2E9A818B7')),
    '必须请求 kugou song/info 按 rawID 精确取');
  assert.ok(!upstreamCalls.some(u => u.includes('/search')), '精确命中不应再走模糊搜索');
});

test('cover-search：精确取失败（cover 空）回退标题搜索', async () => {
  upstreamCalls = [];
  songInfoResp = { name: '晴天', cover: '' };   // 无 cover → 回退
  searchResp = { items: [{ source: 'qq', id: '0039MnYb0q', name: '晴天',
    singer: '周杰伦', cover: 'http://img.qq/qt.jpg' }] };
  const res = await get('/cover-search', 'title=%E6%99%B4%E5%A4%A9&artist=%E5%91%A8%E6%9D%B0%E4%BC%A6&source=kugou&raw_id=BAD');
  assert.equal(res.statusCode, 200);
  const out = JSON.parse(res.body);
  assert.equal(out.cover_url, 'http://img.qq/qt.jpg');
  assert.ok(upstreamCalls.some(u => u.includes('/search')), '精确失败应回退模糊搜索');
});

test('cover-search：无 source/raw_id 走标题搜索（向后兼容）', async () => {
  upstreamCalls = [];
  // 用独立 title 避免命中前序用例的 coverCache（key=cover|title|artist）
  searchResp = { items: [{ source: 'netease', id: '2', name: '稻香',
    singer: '周杰伦', cover: 'http://img.netease/dc.jpg' }] };
  const res = await get('/cover-search', 'title=%E7%A8%BB%E9%A6%99&artist=%E5%91%A8%E6%9D%B0%E4%BC%A6');
  assert.equal(res.statusCode, 200);
  assert.equal(JSON.parse(res.body).cover_url, 'http://img.netease/dc.jpg');
  assert.ok(!upstreamCalls.some(u => u.includes('/song/info')), '无 rawID 不应请求 song/info');
});

test('cover-search：kwqq 伪源映射为 kugou 按 rawID 精确取封面', async () => {
  upstreamCalls = [];
  songInfoResp = { name: '晴天', singer: '周杰伦', cover: 'http://img.kugou/qt-exact.jpg' };
  // source_data.s='kwqq'（CCH 上报伪源），rawID 是酷狗 hash；用独立 hash 避免命中
  // 前序用例 cover|id|kugou|B3A5... 的缓存
  const res = await get('/cover-search', 'title=%E6%99%B4%E5%A4%A9&source=kwqq&raw_id=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA');
  assert.equal(res.statusCode, 200);
  assert.equal(JSON.parse(res.body).cover_url, 'http://img.kugou/qt-exact.jpg');
  assert.ok(upstreamCalls.some(u => u.includes('/kugou/song/info?id=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA')),
    'kwqq 应映射为 kugou 走精确分支');
  assert.ok(!upstreamCalls.some(u => u.includes('/search')), '精确命中不应回退标题搜索');
});

test('cover-search：migu 跳过精确分支（缺 copyright）直接回退标题搜索', async () => {
  upstreamCalls = [];
  songInfoResp = { cover: 'http://wrong/migu.jpg' };
  searchResp = { items: [{ source: 'kugou', id: '9', name: '稻香2',
    singer: '周杰伦', cover: 'http://img.kugou/dc2.jpg' }] };
  const res = await get('/cover-search', 'title=%E7%A8%BB%E9%A6%992&artist=%E5%91%A8%E6%9D%B0%E4%BC%A6&source=migu&raw_id=123');
  assert.equal(res.statusCode, 200);
  assert.equal(JSON.parse(res.body).cover_url, 'http://img.kugou/dc2.jpg');
  assert.ok(!upstreamCalls.some(u => u.includes('/song/info')), 'migu 不应走缺 copyright 的精确分支');
});

test('cover-search：非法 source 不拼入 URL，回退标题搜索', async () => {
  upstreamCalls = [];
  searchResp = { items: [{ source: 'kugou', id: '8', name: '稻香3',
    singer: '周杰伦', cover: 'http://img.kugou/dc3.jpg' }] };
  const res = await get('/cover-search', 'title=%E7%A8%BB%E9%A6%993&artist=%E5%91%A8%E6%9D%B0%E4%BC%A6&source=evil%2Fpath&raw_id=123');
  assert.equal(res.statusCode, 200);
  assert.equal(JSON.parse(res.body).cover_url, 'http://img.kugou/dc3.jpg');
  assert.ok(!upstreamCalls.some(u => u.includes('/evil')), '非法 source 不得进入请求路径');
});
