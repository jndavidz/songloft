package managed

// 架构 §11.4「TTL 恢复：故障注入 200 次；旧表示拼接 0 次」的可执行版本。
//
// 每一轮做四件事，全部走真实 HTTP + 真实票据存储，只把时钟换成可拨动的注入点
// （TicketIssuer.Now 是生产代码里已有的时钟缝，不为了测试新增任何后门）：
//
//  1. 起播后**中途注入票据过期** ⇒ 在途流必须被 guard 立刻掐断，响应短于
//     Content-Length 承诺（失败可见，绝不静默「播完了」）。
//  2. 过期票据的后续请求（含 Range 续传）必须 401，且不泄漏任何媒体字节。
//  3. 恢复路径 = 重新签票后**整份重取** ⇒ 字节哈希等于该表示的完整内容。
//  4. 「旧表示拼接 0 次」的可判定形式：半程截断的前段 + 之后 Range 续传的后段
//     必须仍能拼回**同一份**表示的字节（同一 ETag、逐字节相等）。若服务端在
//     TTL 抖动中给过第二个表示（换字节、换尺寸、换 ETag），这条断言即红。
//
// 200 轮是 §11.4 的样本量；轮内不 sleep、不依赖真实时间，跑完约数秒。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const ttlInjectionRounds = 200

// ttlClock 是可拨动的注入时钟：valid 时返回 base，翻到 expired 后返回 base+2h。
type ttlClock struct {
	base    time.Time
	expired atomic.Bool
}

func (c *ttlClock) now() time.Time {
	if c.expired.Load() {
		return c.base.Add(2 * time.Hour)
	}
	return c.base
}

func TestTTLRecovery_Injection200Rounds(t *testing.T) {
	iss, ms, rep := testIssuer(t)
	srv := wrapMedia(t, ms)

	// 用 2 MiB 确定性内容替换夹具，确保响应跨多次 Read 拷贝——「中途掐断」才可能
	// 落在流中间，而不是第一跳就结束。
	finalPath, err := ms.resolveRepresentationPath(rep)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 2<<20)
	for i := range body {
		body[i] = byte(i % 251)
	}
	if err := os.WriteFile(finalPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	wantETag := repETag(rep, int64(len(body)))
	wantHash := sha256.Sum256(body)

	clock := &ttlClock{base: time.Unix(1_800_000_000, 0)}
	iss.Now = clock.now
	iss.DefaultTTL = time.Minute
	iss.RenewCap = 12 * time.Hour

	var aborts, buffered, spliceChecks, renewals int
	for round := 0; round < ttlInjectionRounds; round++ {
		clock.expired.Store(false)
		tk := issueFor(t, iss, rep)

		// 1) 起播：读前 4096 字节，确认票据此刻有效且表示身份正确。
		resp := getMedia(t, srv, "/media?media_ticket="+tk.Token)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("轮 %d：有效票据应 200，got %d", round, resp.StatusCode)
		}
		if got := strings.Trim(resp.Header.Get("ETag"), `"`); got != wantETag {
			t.Fatalf("轮 %d：ETag=%q want %q（同一表示必须同身份）", round, got, wantETag)
		}
		head := make([]byte, 4096)
		if _, err := io.ReadFull(resp.Body, head); err != nil {
			t.Fatalf("轮 %d：首段读取失败: %v", round, err)
		}

		// 2) 注入：票据到期。在途流要么立刻掐断（读错误/短读，且已交付字节是表示的
		//    前缀），要么在注入前就已完整交付——**绝不**出现「残缺却被当成成功」。
		clock.expired.Store(true)
		rest, err := io.ReadAll(resp.Body)
		delivered := append(append([]byte{}, head...), rest...)
		if err != nil {
			aborts++
			if !bytes.Equal(delivered, body[:len(delivered)]) {
				t.Fatalf("轮 %d：掐断前交付的字节污染了表示", round)
			}
			if len(delivered) >= len(body) {
				t.Fatalf("轮 %d：报告断错但字节已完整", round)
			}
		} else if !bytes.Equal(delivered, body) {
			t.Fatalf("轮 %d：票据过期后交付了既非完整、也非可见中断的内容（静默残缺）", round)
		} else {
			buffered++ // 注入前已全部缓冲：内容仍是同一完整表示，无害
		}
		resp.Body.Close()

		// 3) 过期票据的后续请求（裸 GET 与 Range 续传）：401；响应只有错误文案，
		//    不含任何媒体字节（媒体表示泄漏的判据是长度上限，不是零——见 http.Error）。
		for _, rng := range []string{"", "bytes=4096-"} {
			req, _ := http.NewRequest(http.MethodGet, srv.url+"/media?media_ticket="+tk.Token, nil)
			if rng != "" {
				req.Header.Set("Range", rng)
			}
			rr, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("轮 %d：过期请求失败: %v", round, err)
			}
			n, _ := io.Copy(io.Discard, rr.Body)
			rr.Body.Close()
			if rr.StatusCode != http.StatusUnauthorized {
				t.Fatalf("轮 %d：过期票据应 401（range=%q），got %d", round, rng, rr.StatusCode)
			}
			if n >= 4096 {
				t.Fatalf("轮 %d：过期拒绝泄漏了 %d 字节媒体内容（range=%q）", round, n, rng)
			}
		}

		// 4) 恢复：时钟回正 + 新票 ⇒ 整份重取必须逐字节等于该表示。
		clock.expired.Store(false)
		fresh := issueFor(t, iss, rep)
		fullResp := getMedia(t, srv, "/media?media_ticket="+fresh.Token)
		full, err := io.ReadAll(fullResp.Body)
		fullResp.Body.Close()
		if err != nil {
			t.Fatalf("轮 %d：恢复后整份读取失败: %v", round, err)
		}
		got := sha256.Sum256(full)
		if !bytes.Equal(full, body) || got != wantHash {
			t.Fatalf("轮 %d：恢复后的表示不符（hash %s want %s）", round, hex.EncodeToString(got[:]), hex.EncodeToString(wantHash[:]))
		}

		// 5) 拼接判据：前段（本轮截断处）+ 后段（Range 续传）必须仍是同一份表示。
		rangeResp := rangeMedia(t, srv, fresh.Token, "bytes=4096-")
		if rangeResp.StatusCode != http.StatusPartialContent {
			t.Fatalf("轮 %d：Range 续传应 206，got %d", round, rangeResp.StatusCode)
		}
		if got := strings.Trim(rangeResp.Header.Get("ETag"), `"`); got != wantETag {
			t.Fatalf("轮 %d：续传换了 ETag（第二个表示）: %q", round, got)
		}
		tail, err := io.ReadAll(rangeResp.Body)
		rangeResp.Body.Close()
		if err != nil {
			t.Fatalf("轮 %d：续传读取失败: %v", round, err)
		}
		joined := append(append([]byte{}, head...), tail...)
		if !bytes.Equal(joined, body) {
			t.Fatalf("轮 %d：旧段 + 新段拼不回同一表示（拼接故障）", round)
		}
		spliceChecks++

		// 6) 未过期时续期：URI/表示不变、token 不换、可继续取。
		if _, err := iss.Renew(context.Background(), fresh.Token, 20*time.Second); err != nil {
			t.Fatalf("轮 %d：未过期票据续期失败: %v", round, err)
		}
		renewals++
		after := getMedia(t, srv, "/media?media_ticket="+fresh.Token)
		if after.StatusCode != http.StatusOK {
			t.Fatalf("轮 %d：续期后同一票据应 200，got %d", round, after.StatusCode)
		}
		after.Body.Close()
	}
	t.Logf("TTL 注入完成：%d 轮 / 在途掐断 %d / 注入前已缓冲完 %d / 拼接校验 %d / 续期 %d",
		ttlInjectionRounds, aborts, buffered, spliceChecks, renewals)
	if spliceChecks != ttlInjectionRounds || renewals != ttlInjectionRounds {
		t.Fatalf("轮数未跑满：splice=%d renew=%d want %d", spliceChecks, renewals, ttlInjectionRounds)
	}
	// 非空洞校验：200 轮里至少要真掐断一次在途流，否则本用例只是在测「永不失败」。
	if aborts == 0 {
		t.Fatal("200 轮注入未产生一次在途掐断：夹具失效，本用例不构成证据")
	}
}

// rangeMedia 发一次带 Range 的 GET（调用方负责关闭 body）。
func rangeMedia(t *testing.T, srv *mediaSrv, token, rng string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.url+"/media?media_ticket="+token, nil)
	req.Header.Set("Range", rng)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET range: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}
