package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestManagedState_ReadThroughBridge：已 handoff 的会话，/managed/state
// 必须返回 bridge 快照的只读投影（P4 / ticket 05a）。
func TestManagedState_ReadThroughBridge(t *testing.T) {
	h, _ := newBridgeHandler(t)
	// 模拟已 handoff：session|epoch=1 有 bridge sid + entry 映射。
	h.bridgeStates["s1|1"] = &bridgeKeyState{
		sid: "ms-9", entries: map[string]string{"e1": "e-b1", "e2": "e-b2"},
	}
	req := httptest.NewRequest(http.MethodGet, "/managed/state?session=s1", nil)
	rec := httptest.NewRecorder()
	h.State(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("state http=%d", rec.Code)
	}
	var out struct {
		Queue []struct {
			Entry string `json:"entry"`
		} `json:"Queue"`
		Current int `json:"Current"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	fmt.Println(rec.Body.String())
	if len(out.Queue) != 2 {
		t.Fatalf("Queue 应来自 bridge 快照（2 项），got %d: %s", len(out.Queue), rec.Body.String())
	}
	if out.Queue[0].Entry != "e1" || out.Queue[1].Entry != "e2" {
		t.Errorf("entry 映射应翻回 BFF entry，got %+v", out.Queue)
	}
	if out.Current != 1 {
		t.Errorf("Current 应来自 bridge current_index=1，got %d", out.Current)
	}
}
