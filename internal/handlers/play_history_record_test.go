package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"songloft/internal/database/testutil"
	"songloft/internal/models"
	"songloft/internal/services"

	"github.com/go-chi/chi/v5"
)

// TestSongPlayedRecordsHistoryOnlyWithContext 覆盖 POST /songs/{id}/played 的播放历史副作用：
// 只有 type=play 且 context_type + context_key 都非空时才落库。
//
// 这条断言是给「播放历史一直空白」定位用的：该端点无论是否落库都返回 204，
// 光看访问日志/HTTP 状态码无法区分「记了」与「被跳过」。
// 客户端不传 context（如曲库列表直接点播）时，历史里就该是 0 条。
func TestSongPlayedRecordsHistoryOnlyWithContext(t *testing.T) {
	mdb := testutil.OpenMemoryDB(t)
	repo := mdb.SongRepository()
	songService := services.NewSongService(repo, nil, nil, nil, nil, nil)
	h := NewSongHandler(songService, nil, nil, nil, nil, nil)
	h.SetPlayHistoryRecorder(services.NewPlayHistoryService(mdb))

	songID := seedSong(t, repo, &models.Song{Type: models.TypeLocal, Title: "历史测试", FilePath: "/music/test.mp3"})

	router := chi.NewRouter()
	router.Post("/songs/{id}/played", h.SongPlayed)

	doPlayed := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/songs/%d/played%s", songID, query), nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// 1) 带完整上下文：落库 1 条。
	if rec := doPlayed("?type=play&context_type=playlist&context_key=1"); rec.Code != http.StatusNoContent {
		t.Fatalf("with context: status = %d, want 204", rec.Code)
	}
	if n, err := mdb.PlayHistoryRepository().Count(context.Background(), models.PlayContextPlaylist, "1"); err != nil {
		t.Fatalf("count playlist history: %v", err)
	} else if n != 1 {
		t.Fatalf("with context: history rows = %d, want 1", n)
	}

	// 2) 不带上下文（客户端缺 PlaybackContext 的真实情形）：仍返回 204，但不落库。
	if rec := doPlayed("?type=play"); rec.Code != http.StatusNoContent {
		t.Fatalf("without context: status = %d, want 204", rec.Code)
	}
	if n, err := mdb.PlayHistoryRepository().Count(context.Background(), models.PlayContextPlaylist, "1"); err != nil {
		t.Fatalf("count playlist history: %v", err)
	} else if n != 1 {
		t.Fatalf("without context: history rows = %d, want still 1", n)
	}

	// 3) type=finish 即使带上下文也不落库（同一首歌的重复事件）。
	if rec := doPlayed("?type=finish&context_type=playlist&context_key=2"); rec.Code != http.StatusNoContent {
		t.Fatalf("finish: status = %d, want 204", rec.Code)
	}
	if n, err := mdb.PlayHistoryRepository().Count(context.Background(), models.PlayContextPlaylist, "2"); err != nil {
		t.Fatalf("count playlist history: %v", err)
	} else if n != 0 {
		t.Fatalf("finish: history rows = %d, want 0", n)
	}

	// 4) 曲库浏览上下文 (browse, 音源标识) 必须被接受——客户端插件音源点播走这条路，
	//    曾经因为服务端白名单只认 playlist/tag/分面而被静默丢弃（接口照样回 204）。
	if rec := doPlayed("?type=play&context_type=browse&context_key=kwqq"); rec.Code != http.StatusNoContent {
		t.Fatalf("browse: status = %d, want 204", rec.Code)
	}
	if n, err := mdb.PlayHistoryRepository().Count(context.Background(), models.PlayContextBrowse, "kwqq"); err != nil {
		t.Fatalf("count browse history: %v", err)
	} else if n != 1 {
		t.Fatalf("browse: history rows = %d, want 1", n)
	}
}
