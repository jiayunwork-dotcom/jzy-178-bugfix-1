package server_test

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"raim/internal/sim"
	"raim/pkg/profile"
	"raim/pkg/server"
	"raim/pkg/session"
)

func epochInputs(c *sim.Constellation, n int, seed int64,
	biasAt func(int) map[int]float64) []session.EpochInput {
	rng := rand.New(rand.NewSource(seed))
	out := make([]session.EpochInput, n)
	for i := range out {
		var bias map[int]float64
		if biasAt != nil {
			bias = biasAt(i)
		}
		ep := c.Observe(sim.Obs{Rng: rng, Sigma: 1, Bias: bias})
		sats := make([]session.SatInput, len(ep.Sats))
		for j, s := range ep.Sats {
			sats[j] = session.SatInput{ID: s.ID, Pos: [3]float64{s.Pos.X, s.Pos.Y, s.Pos.Z}, PR: s.PR, Sigma: s.Sigma}
		}
		out[i] = session.EpochInput{Timestamp: int64(i + 1), Sats: sats}
	}
	return out
}

func start(t *testing.T, dir string) (*server.Server, *httptest.Server) {
	t.Helper()
	store, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(store)
	ts := httptest.NewServer(srv.Echo())
	return srv, ts
}

func TestHTTPEndToEnd(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	_, ts := start(t, dir)
	defer ts.Close()

	// 列出内置运行档
	resp := mustGet(t, ts.URL+"/api/profiles")
	ps := resp["profiles"].([]any)
	if len(ps) != 3 {
		t.Fatalf("内置档应为 3, got %v", len(ps))
	}

	// 新建自定义档
	custom := profile.Profile{
		Name: "custom", Pfa: 1e-6, Pmd: 1e-4, HAL: 800,
		Isolation: profile.Isolation{MinEpochs: 4},
		Alert:     profile.Alert{Mode: profile.Persistence, ConfirmEpochs: 2, ClearEpochs: 2},
	}
	if code := postCode(t, ts.URL+"/api/profiles", custom); code != http.StatusCreated {
		t.Fatalf("建档状态码 %d", code)
	}

	// 建会话
	resp = postBody(t, ts.URL+"/api/sessions", map[string]string{"profile_name": "custom"}, http.StatusCreated)
	if resp["profile_name"] != "custom" {
		t.Fatalf("会话档=%v", resp["profile_name"])
	}
	id := resp["id"].(string)

	// 逐历元提交 3 个
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)
	epochs := epochInputs(c, 3, 5, nil)
	for _, e := range epochs {
		postBody(t, ts.URL+"/api/sessions/"+id+"/epochs", e, http.StatusOK)
	}

	// 重复时间戳 -> 409
	code := postCode(t, ts.URL+"/api/sessions/"+id+"/epochs", epochs[2])
	if code != http.StatusConflict {
		t.Fatalf("重复时间戳应 409, got %d", code)
	}
	// 倒退 -> 422
	back := epochs[0]
	back.Timestamp = 0
	if code := postCode(t, ts.URL+"/api/sessions/"+id+"/epochs", back); code != http.StatusUnprocessableEntity {
		t.Fatalf("倒退时间戳应 422, got %d", code)
	}

	// 校验错误指向字段：4 颗星
	four := epochs[0]
	four.Timestamp = 100
	four.Sats = four.Sats[:3]
	badResp := postBody(t, ts.URL+"/api/sessions/"+id+"/epochs", four, http.StatusBadRequest)
	if badResp["field"] != "satellites" {
		t.Fatalf("错误字段=%v", badResp["field"])
	}

	// 会话查询：统计仍为 3（拒收未推进）
	resp = mustGet(t, ts.URL+"/api/sessions/"+id)
	stats := resp["stats"].(map[string]any)
	if int(stats["epochs"].(float64)) != 3 {
		t.Fatalf("历元数应为 3, got %v", stats["epochs"])
	}

	// 未知会话 -> 404
	if code := getCode(t, ts.URL+"/api/sessions/nope"); code != http.StatusNotFound {
		t.Fatalf("未知会话应 404, got %d", code)
	}
}

func TestHTTPBatchEqualsIndividualAcrossRestart(t *testing.T) {
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)
	epochs := epochInputs(c, 30, 9, func(i int) map[int]float64 {
		if i >= 6 && i <= 15 {
			return map[int]float64{4: 80}
		}
		return nil
	})

	// A：批量一次提交
	dirA := filepath.Join(t.TempDir(), "dataA")
	_, tsA := start(t, dirA)
	postBody(t, tsA.URL+"/api/sessions", map[string]string{"id": "a", "profile_name": "enroute"}, http.StatusCreated)
	batchResp := postBody(t, tsA.URL+"/api/sessions/a/epochs/batch",
		map[string]any{"epochs": epochs}, http.StatusOK)
	respA := mustGet(t, tsA.URL+"/api/sessions/a")
	tsA.Close()
	_ = batchResp

	// B：逐历元提交，中途重启（新服务指向同一目录）
	dirB := filepath.Join(t.TempDir(), "dataB")
	_, tsB := start(t, dirB)
	postBody(t, tsB.URL+"/api/sessions", map[string]string{"id": "b", "profile_name": "enroute"}, http.StatusCreated)
	for _, e := range epochs[:12] {
		postBody(t, tsB.URL+"/api/sessions/b/epochs", e, http.StatusOK)
	}
	tsB.Close()
	_, tsB2 := start(t, dirB) // 重启续跑
	for _, e := range epochs[12:] {
		postBody(t, tsB2.URL+"/api/sessions/b/epochs", e, http.StatusOK)
	}

	respB := mustGet(t, tsB2.URL+"/api/sessions/b")
	defer tsB2.Close()

	ra := recsOf(respA)
	rb := recsOf(respB)
	if len(ra) != len(rb) {
		t.Fatalf("记录数不同 %d vs %d", len(ra), len(rb))
	}
	for i := range ra {
		ba, _ := json.Marshal(ra[i])
		bb, _ := json.Marshal(rb[i])
		if !bytes.Equal(ba, bb) {
			t.Fatalf("历元 %d 批量与逐历元(含重启)结果不一致:\nA=%s\nB=%s", i, ba, bb)
		}
	}
	if jsonEq(respA["stats"], respB["stats"]) == false {
		t.Fatalf("统计不一致:\nA=%v\nB=%v", respA["stats"], respB["stats"])
	}
}

func jsonEq(a, b any) bool {
	ba, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ba, bb)
}

func recsOf(resp map[string]any) []any {
	return resp["records"].([]any)
}

// ---- small http helpers ----

func mustGet(t *testing.T, url string) map[string]any {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func getCode(t *testing.T, url string) int {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	return r.StatusCode
}

func postBody(t *testing.T, url string, body any, want int) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	r, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	if r.StatusCode != want {
		t.Fatalf("POST %s 状态码 %d, 期望 %d, body=%v", url, r.StatusCode, want, m)
	}
	return m
}

func postCode(t *testing.T, url string, body any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	r, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	return r.StatusCode
}
