package session_test

import (
	"errors"
	"math"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"raim/internal/sim"
	"raim/pkg/apierr"
	"raim/pkg/lsq"
	"raim/pkg/profile"
	"raim/pkg/session"
)

func toInput(ts int64, ep *lsq.Epoch) session.EpochInput {
	sats := make([]session.SatInput, len(ep.Sats))
	for i, s := range ep.Sats {
		sats[i] = session.SatInput{
			ID:    s.ID,
			Pos:   [3]float64{s.Pos.X, s.Pos.Y, s.Pos.Z},
			PR:    s.PR,
			Sigma: s.Sigma,
		}
	}
	ap := [3]float64{ep.Approx.X, ep.Approx.Y, ep.Approx.Z}
	return session.EpochInput{Timestamp: ts, Approx: &ap, Sats: sats}
}

func genEpochs(c *sim.Constellation, n int, sigma float64, seed int64,
	biasAt func(seq0 int) map[int]float64) []session.EpochInput {
	rng := rand.New(rand.NewSource(seed))
	out := make([]session.EpochInput, n)
	for i := range out {
		var bias map[int]float64
		if biasAt != nil {
			bias = biasAt(i)
		}
		ep := c.Observe(sim.Obs{Rng: rng, Sigma: sigma, Bias: bias})
		out[i] = toInput(int64(i+1), ep)
	}
	return out
}

func faultWindow(badID int, from, to int, bias float64) func(int) map[int]float64 {
	return func(seq int) map[int]float64 {
		if seq >= from && seq <= to {
			return map[int]float64{badID: bias}
		}
		return nil
	}
}

func assertRecordsEqual(t *testing.T, a, b []*session.EpochRecord) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("历元数不同 %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !reflect.DeepEqual(a[i], b[i]) {
			t.Fatalf("历元 %d (ts=%d) 记录不一致:\n A=%+v\n B=%+v", i, a[i].Timestamp, a[i], b[i])
		}
	}
}

func assertStateEqual(t *testing.T, a, b *session.Session) {
	t.Helper()
	if !reflect.DeepEqual(a.Stats, b.Stats) {
		t.Fatalf("统计不一致:\n A=%+v\n B=%+v", a.Stats, b.Stats)
	}
	// 比较隔离星编号集合与内容
	if len(a.State.Isolated) != len(b.State.Isolated) {
		t.Fatalf("隔离星数量不同: %v vs %v",
			sortedIsolatedIDs(a), sortedIsolatedIDs(b))
	}
	for id, ea := range a.State.Isolated {
		eb := b.State.Isolated[id]
		if eb == nil || *ea != *eb {
			t.Fatalf("隔离星 %d 状态不同: %+v vs %+v", id, ea, eb)
		}
	}
	if a.State.Alert != b.State.Alert {
		t.Fatalf("告警状态不同: %+v vs %+v", a.State.Alert, b.State.Alert)
	}
}

func sortedIsolatedIDs(s *session.Session) []int {
	var ids []int
	for id := range s.State.Isolated {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func newStore(t *testing.T) *session.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := session.NewStore(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestBatchEqualsIndividual(t *testing.T) {
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)
	epochs := genEpochs(c, 40, 1, 2024, faultWindow(4, 8, 20, 80))

	// A：一次性批量
	stA := newStore(t)
	if _, err := stA.CreateSession("s", "enroute"); err != nil {
		t.Fatal(err)
	}
	recsA, err := stA.AppendBatch("s", epochs)
	if err != nil {
		t.Fatal(err)
	}
	sessA, _ := stA.GetSession("s")

	// B：逐历元
	stB := newStore(t)
	if _, err := stB.CreateSession("s", "enroute"); err != nil {
		t.Fatal(err)
	}
	var recsB []*session.EpochRecord
	for _, e := range epochs {
		r, err := stB.AppendEpoch("s", e)
		if err != nil {
			t.Fatal(err)
		}
		recsB = append(recsB, r)
	}
	sessB, _ := stB.GetSession("s")

	assertRecordsEqual(t, recsA, recsB)
	assertRecordsEqual(t, sessA.Records, sessB.Records)
	assertStateEqual(t, sessA, sessB)
}

func TestRestartResumeEqualsContinuous(t *testing.T) {
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)
	epochs := genEpochs(c, 40, 1, 7, faultWindow(4, 8, 20, 80))

	dir := filepath.Join(t.TempDir(), "data")

	// 一口气跑完
	stFull, err := session.NewStore(dir + "_full")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stFull.CreateSession("s", "enroute"); err != nil {
		t.Fatal(err)
	}
	if _, err := stFull.AppendBatch("s", epochs); err != nil {
		t.Fatal(err)
	}
	sessFull, _ := stFull.GetSession("s")

	// 分段 + 每段用“新 Store”（模拟重启，从磁盘重新加载）
	const split = 17
	st1, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st1.CreateSession("s", "enroute"); err != nil {
		t.Fatal(err)
	}
	for _, e := range epochs[:split] {
		if _, err := st1.AppendEpoch("s", e); err != nil {
			t.Fatal(err)
		}
	}
	// “重启”：换一个 Store 实例指向同一目录
	st2, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range epochs[split:] {
		if _, err := st2.AppendEpoch("s", e); err != nil {
			t.Fatal(err)
		}
	}
	sessResumed, _ := st2.GetSession("s")

	assertRecordsEqual(t, sessResumed.Records, sessFull.Records)
	assertStateEqual(t, sessResumed, sessFull)
	if sessResumed.LastTS == nil || *sessResumed.LastTS != int64(len(epochs)) {
		t.Fatalf("续跑后 last_ts=%v", sessResumed.LastTS)
	}
}

func TestDuplicateAndStaleTimestamp(t *testing.T) {
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)
	epochs := genEpochs(c, 5, 1, 3, nil)
	st := newStore(t)
	if _, err := st.CreateSession("s", "enroute"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEpoch("s", epochs[0]); err != nil {
		t.Fatal(err)
	}
	sess, _ := st.GetSession("s")
	if sess.Stats.Epochs != 1 {
		t.Fatal("初始应为 1 历元")
	}
	// 重复时间戳
	if _, err := st.AppendEpoch("s", epochs[0]); !errors.Is(err, apierr.ErrDuplicateEpoch) {
		t.Fatalf("重复时间戳应拒收, got %v", err)
	}
	// 倒退时间戳
	back := epochs[0]
	back.Timestamp = 0
	if _, err := st.AppendEpoch("s", back); !errors.Is(err, apierr.ErrStaleEpoch) {
		t.Fatalf("倒退时间戳应收, got %v", err)
	}
	// 状态未被推进
	sess, _ = st.GetSession("s")
	if sess.Stats.Epochs != 1 {
		t.Fatalf("拒收后历元数不应变, got %d", sess.Stats.Epochs)
	}
	// 正常推进
	if _, err := st.AppendEpoch("s", epochs[1]); err != nil {
		t.Fatal(err)
	}
	sess, _ = st.GetSession("s")
	if sess.Stats.Epochs != 2 {
		t.Fatalf("应为 2 历元, got %d", sess.Stats.Epochs)
	}
}

func customProfile(name string, mode profile.AlertMode, minIso, confirm, clear int, gross float64) profile.Profile {
	return profile.Profile{
		Name: name, Pfa: 1e-5, Pmd: 1e-3, HAL: 1e9, // 本测试不触发 HPL 告警
		Isolation: profile.Isolation{MinEpochs: minIso},
		Alert:     profile.Alert{Mode: mode, ConfirmEpochs: confirm, ClearEpochs: clear, GrossFactor: gross},
	}
}

func TestIsolationHoldAndRecover(t *testing.T) {
	// 快照告警、隔离恢复需连续 4 历元
	const badID = 5
	const minIso = 4
	prof := customProfile("iso", profile.Snapshot, minIso, 1, 1, 0)
	svc := session.NewService([]profile.Profile{prof})
	sess, err := svc.NewSession("s", "iso")
	if err != nil {
		t.Fatal(err)
	}
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)
	// 第 5 历元（seq=4）起到第 12 历元（seq=11）注入 80m 偏差
	epochs := genEpochs(c, 20, 1, 11, faultWindow(badID, 4, 11, 80))

	var recs []*session.EpochRecord
	for _, e := range epochs {
		r, err := svc.Step(sess, e)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}

	// 首个故障历元：恰好排除该星
	first := recs[4]
	if first.Mode != "excluded" || first.ExcludedID != badID {
		t.Fatalf("首个故障历元应排除 #%d, mode=%s excluded=%d", badID, first.Mode, first.ExcludedID)
	}
	// 从下一历元起到偏差结束（历元12）始终隔离、且不参与解算（不反复进出）
	for i := 5; i <= 11; i++ {
		r := recs[i]
		if !contains(r.Isolated, badID) {
			t.Fatalf("历元 %d 坏星不应被拉回（隔离列表=%v）", i+1, r.Isolated)
		}
		for _, sr := range r.SatResults {
			if sr.ID == badID {
				t.Fatalf("历元 %d 隔离星竟参与解算", i+1)
			}
		}
	}
	// 偏差撤掉（历元13=seq12 起）：连续 minIso 个历元自身正常且加回通过，
	// 历元13/14/15 streak=1/2/3 仍隔离，历元16 streak=4 解除。
	for i := 12; i <= 14; i++ {
		if !contains(recs[i].Isolated, badID) {
			t.Fatalf("历元 %d 恢复期未满，应仍隔离", i+1)
		}
	}
	recoverEpoch := 15 // 0-based：历元16
	if contains(recs[recoverEpoch].Isolated, badID) {
		t.Fatalf("历元 %d 应已解除隔离", recoverEpoch+1)
	}
	// 恢复历元的活动星集合在解除前已确定，故该星在下一历元重新参与解算
	found := false
	for _, sr := range recs[recoverEpoch+1].SatResults {
		if sr.ID == badID {
			found = true
		}
	}
	if !found {
		t.Fatal("恢复后下一历元坏星应重新参与解算")
	}
	// 故障期间位置始终在真值附近（隔离生效，未被坏星带偏）
	for i := 4; i < 12; i++ {
		p := recs[i].Position.ECEF
		d := norm3(p, [3]float64{rec.Pos.X, rec.Pos.Y, rec.Pos.Z})
		if d > 10 {
			t.Fatalf("历元 %d 定位偏差 %.2f m，隔离未生效", i+1, d)
		}
	}
}

func TestFaultWindowStatsAndAvailability(t *testing.T) {
	prof := customProfile("p", profile.Snapshot, 3, 1, 1, 0)
	svc := session.NewService([]profile.Profile{prof})
	sess, _ := svc.NewSession("s", "p")
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)
	epochs := genEpochs(c, 30, 1, 1, nil) // 全程无故障
	for _, e := range epochs {
		if _, err := svc.Step(sess, e); err != nil {
			t.Fatal(err)
		}
	}
	if sess.Stats.Epochs != 30 {
		t.Fatal("历元数")
	}
	if sess.Stats.Alerts != 0 || sess.Stats.FalseAlarms != 0 {
		t.Fatalf("无故障不应有告警: %+v", sess.Stats)
	}
	if math.Abs(sess.Stats.RAIMAvailRate-1) > 1e-9 {
		t.Fatalf("无故障 RAIM 可用率应为 1, got %v", sess.Stats.RAIMAvailRate)
	}
	if math.Abs(sess.Stats.ServiceAvail-1) > 1e-9 {
		t.Fatalf("无故障服务可用率应为 1, got %v", sess.Stats.ServiceAvail)
	}
}

func TestBatchTooLarge(t *testing.T) {
	st := newStore(t)
	if _, err := st.CreateSession("s", "enroute"); err != nil {
		t.Fatal(err)
	}
	big := make([]session.EpochInput, 3601)
	_, err := st.AppendBatch("s", big)
	fe, ok := err.(*apierr.FieldError)
	if !ok || fe.Field != "epochs" {
		t.Fatalf("应指向 epochs 字段, got %v", err)
	}
}

func TestBatchFieldErrorPrefixed(t *testing.T) {
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)
	epochs := genEpochs(c, 5, 1, 1, nil)
	// 第 3 个历元制造 NaN 伪距
	epochs[2].Sats[1].PR = math.NaN()
	st := newStore(t)
	if _, err := st.CreateSession("s", "enroute"); err != nil {
		t.Fatal(err)
	}
	_, err := st.AppendBatch("s", epochs)
	fe, ok := err.(*apierr.FieldError)
	if !ok {
		t.Fatalf("期望字段错误, got %v", err)
	}
	if fe.Field != "epochs[2].satellites[1].pr" {
		t.Fatalf("错误字段=%s", fe.Field)
	}
	// 整批拒收：会话仍为空
	sess, _ := st.GetSession("s")
	if sess.Stats.Epochs != 0 {
		t.Fatalf("整批失败不应推进状态, epochs=%d", sess.Stats.Epochs)
	}
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func norm3(a, b [3]float64) float64 {
	var s float64
	for i := 0; i < 3; i++ {
		d := a[i] - b[i]
		s += d * d
	}
	return math.Sqrt(s)
}

var _ = sort.Ints
