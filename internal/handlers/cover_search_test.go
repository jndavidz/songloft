package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"songloft/internal/models"
	"songloft/internal/services"
)

// stubCoverSearcher 记录 SearchCover 收到的 source/rawID，返回给定封面 URL。
type stubCoverSearcher struct {
	gotSource, gotRawID string
	returnURL           string
}

func (s *stubCoverSearcher) SearchCover(_ context.Context, title, artist, album string,
	_ string, _ string, source, rawID string) (string, error) {
	s.gotSource, s.gotRawID = source, rawID
	return s.returnURL, nil
}

// TestGetSongCoverPassesSourceIdentity：cover_url 为空时，GetSongCover 必须把
// source_data 里的平台(s)与 rawID(i) 传给封面搜索器，让插件可按 rawID 精确取封面。
// 这是「收藏曲按 rawID 取封面」方案的服务端契约守门（§2.77-50）。
func TestGetSongCoverPassesSourceIdentity(t *testing.T) {
	// 假封面源：返回一张图片
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		fmt.Fprint(w, "cover-bytes")
	}))
	defer img.Close()

	repo := newTestSongRepo(t)
	service := services.NewSongService(repo, nil, nil, nil, nil, nil)
	h := NewSongHandler(service, nil, nil, nil, nil, nil)
	stub := &stubCoverSearcher{returnURL: img.URL + "/cover.jpg"}
	h.SetCoverSearcher(stub)

	// 经典 kwqq source_data：i 是字符串（32 位 hash）
	id := seedSong(t, repo, &models.Song{
		Title: "晴天", Artist: "周杰伦", Type: models.TypeRemote,
		URL:        "https://example.com/audio",
		SourceData: `{"s":"kugou","i":"B3A52A7A958BF0AED0EBFBA2E9A818B7","n":"晴天"}`,
		// CoverURL / CoverPath 空 → 触发封面搜索分支
	})
	w := httptest.NewRecorder()
	h.GetSongCover(w, newRouteRequest(http.MethodGet, "/api/v1/songs/cover", nil,
		map[string]string{"id": strconv.FormatInt(id, 10)}))

	if stub.gotSource != "kugou" {
		t.Errorf("source = %q, want kugou", stub.gotSource)
	}
	if stub.gotRawID != "B3A52A7A958BF0AED0EBFBA2E9A818B7" {
		t.Errorf("rawID = %q, want B3A52A7A958BF0AED0EBFBA2E9A818B7", stub.gotRawID)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestGetSongCoverNumericRawID：source_data.i 是数字时归一为字符串，且大整数
// 不被 float64 科学计数法破坏（json.Number 原样保留）——10 位网易 ID 守门。
func TestGetSongCoverNumericRawID(t *testing.T) {
	for _, tc := range []struct {
		name, sourceData, wantID string
	}{
		{"big-int", `{"s":"netease","i":1970511837}`, "1970511837"},
		{"small-int", `{"s":"netease","i":12345}`, "12345"},
		{"bool-invalid", `{"s":"netease","i":true}`, ""},
		{"object-invalid", `{"s":"netease","i":{"x":1}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "image/jpeg")
				fmt.Fprint(w, "cover-bytes")
			}))
			defer img.Close()

			repo := newTestSongRepo(t)
			service := services.NewSongService(repo, nil, nil, nil, nil, nil)
			h := NewSongHandler(service, nil, nil, nil, nil, nil)
			stub := &stubCoverSearcher{returnURL: img.URL + "/cover.jpg"}
			h.SetCoverSearcher(stub)

			id := seedSong(t, repo, &models.Song{
				Title: "测试", Artist: "歌手", Type: models.TypeRemote,
				URL:        "https://example.com/audio",
				SourceData: tc.sourceData,
			})
			w := httptest.NewRecorder()
			h.GetSongCover(w, newRouteRequest(http.MethodGet, "/api/v1/songs/cover", nil,
				map[string]string{"id": strconv.FormatInt(id, 10)}))

			if stub.gotSource != "netease" || stub.gotRawID != tc.wantID {
				t.Errorf("source/rawID = %q/%q, want netease/%q", stub.gotSource, stub.gotRawID, tc.wantID)
			}
		})
	}
}
