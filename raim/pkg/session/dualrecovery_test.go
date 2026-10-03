package session_test

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"raim/internal/sim"
	"raim/pkg/profile"
	"raim/pkg/session"
)

// isoProfile 试飞组复现用运行档：隔离恢复 4 历元、告警限放到极大。
func isoProfile() profile.Profile {
	return profile.Profile{
		Name: "iso4", Pfa: 1e-3, Pmd: 1e-3, HAL: 1e9,
		Isolation: profile.Isolation{MinEpochs: 4},
		Alert:     profile.Alert{Mode: profile.Snapshot, ConfirmEpochs: 1, ClearEpochs: 1},
	}
}

// dualSky 与试飞组一致：10 颗星、σ=1 m，几何固定种子，噪声固定种子。
func dualSky() *sim.Constellation {
	return sim.EvenSky(sim.DefaultReceiver(), 10, 15, 5)
}

func dualEpochs(prn6Start0 int, prn6End0 int) []session.EpochInput {
	c := dualSky()
	rng := rand.New(rand.NewSource(20260930))
	const n = 40
	out := make([]session.EpochInput, n)
	for i := range out {
		m := map[int]float64{}
		if i >= 4 && i <= 9 { // PRN3：历元 5..10（1-based）
			m[3] = 80
		}
		if i >= prn6Start0-1 { // PRN6：从指定历元起
			if prn6End0 < 0 || i <= prn6End0-1 {
				m[6] = 80
			}
		}
		var bias map[int]float64
		if len(m) > 0 {
			bias = m
		}
		ep := c.Observe(sim.Obs{Rng: rng, Sigma: 1, Bias: bias})
		out[i] = toInput(int64(i+1), ep)
	}
	return out
}

func isoTest(recs []*session.EpochRecord, seq1 int, id int) session.IsolationTest {
	r := recs[seq1-1]
	i := sort.Search(len(r.IsolationTests), func(i int) bool {
		return r.IsolationTests[i].ID >= id
	})
	if i < len(r.IsolationTests) && r.IsolationTests[i].ID == id {
		return r.IsolationTests[i]
	}
	return session.IsolationTest{ID: -1}
}

func assertIsolated(t *testing.T, r *session.EpochRecord, id int, want bool) {
	t.Helper()
	got := contains(r.Isolated, id)
	if got != want {
		t.Fatalf("历元 %d: PRN%d 隔离状态 got=%v want=%v (list=%v)",
			r.Seq, id, got, want, r.Isolated)
	}
}

// TestDualFaultPRN6Persists 第一组：PRN3 历元5..10 偏，PRN6 历元8 起一直偏。
func TestDualFaultPRN6Persists(t *testing.T) {
	svc := session.NewService([]profile.Profile{isoProfile()})
	sess, _ := svc.NewSession("s", "iso4")
	var recs []*session.EpochRecord
	for _, e := range dualEpochs(8, -1) {
		r, err := svc.Step(sess, e)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}

	// 检出/剔除时刻
	if recs[4].Mode != "excluded" || recs[4].ExcludedID != 3 {
		t.Fatalf("历元 5 应唯一剔除 PRN3, got mode=%s excl=%d", recs[4].Mode, recs[4].ExcludedID)
	}
	if recs[7].Mode != "excluded" || recs[7].ExcludedID != 6 {
		t.Fatalf("历元 8 应唯一剔除 PRN6, got mode=%s excl=%d", recs[7].Mode, recs[7].ExcludedID)
	}
	// PRN3 偏差历元 11 撤除，连续正常历元 11/12/13，历元 14 解除（PRN6 不影响其计数）
	assertIsolated(t, recs[11], 3, true) // 历元12 仍隔离
	assertIsolated(t, recs[12], 3, true) // 历元13 仍隔离
	for i := 10; i <= 12; i++ {
		it := isoTest(recs, i+1, 3)
		if it.ID != 3 {
			t.Fatalf("历元 %d 缺 PRN3 隔离检验明细", i+1)
		}
		if !it.Evaluable {
			t.Fatalf("历元 %d PRN3 检验应可评估", i+1)
		}
	}
	if it := isoTest(recs, 11, 3); it.NormalStreak != 1 || !it.Pass {
		t.Fatalf("历元 11 PRN3 streak 应为 1 且通过, got %+v", it)
	}
	if it := isoTest(recs, 12, 3); it.NormalStreak != 2 || !it.Pass {
		t.Fatalf("历元 12 PRN3 streak 应为 2, got %+v", it)
	}
	if it := isoTest(recs, 13, 3); it.NormalStreak != 3 || !it.Pass {
		t.Fatalf("历元 13 PRN3 streak 应为 3, got %+v", it)
	}
	assertIsolated(t, recs[13], 3, false) // 0-based 13 = 历元14 解除
	// 解除后下一历元重新参与解算
	found3 := false
	for _, sr := range recs[14].SatResults {
		if sr.ID == 3 {
			found3 = true
		}
	}
	if !found3 {
		t.Fatal("历元 15 PRN3 应重新参与解算")
	}
	// PRN6 偏差始终在：永不放回，检验量逐历元暴露大偏差、streak 恒 0
	assertIsolated(t, recs[7], 6, true) // 历元 8：刚被唯一排除即进入隔离
	for i := 8; i < 40; i++ {
		assertIsolated(t, recs[i], 6, true)
		it := isoTest(recs, i+1, 6)
		if it.ID != 6 {
			t.Fatalf("历元 %d 缺 PRN6 隔离检验明细", i+1)
		}
		if it.Pass || it.NormalStreak != 0 || it.Statistic < 10 || it.SatPass {
			t.Fatalf("历元 %d PRN6 不应被其他星残差掩护: %+v", i+1, it)
		}
		// 历元 9 起 PRN6 不再参与解算（历元 8 是排除当历元，采用排除解）
		for _, sr := range recs[i].SatResults {
			if sr.ID == 6 {
				t.Fatalf("历元 %d PRN6 不应参与解算", i+1)
			}
		}
	}
	// 最终解算 9 颗星（PRN3 已回、PRN6 仍隔离），HPL 不再是双星缺失值
	if len(recs[39].SatResults) != 9 {
		t.Fatalf("历元 40 参与解算星数=%d, 期望 9", len(recs[39].SatResults))
	}
	if recs[39].HPL >= recs[9].HPL {
		t.Fatalf("PRN3 回来后 HPL 应下降: %.2f vs 双星缺失 %.2f", recs[39].HPL, recs[9].HPL)
	}
}

// TestDualFaultPRN6StartsDuringRecovery 第二组：PRN6 从历元 12（PRN3 正攒恢复历元）起偏。
func TestDualFaultPRN6StartsDuringRecovery(t *testing.T) {
	svc := session.NewService([]profile.Profile{isoProfile()})
	sess, _ := svc.NewSession("s", "iso4")
	var recs []*session.EpochRecord
	for _, e := range dualEpochs(12, -1) {
		r, err := svc.Step(sess, e)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
	if recs[11].Mode != "excluded" || recs[11].ExcludedID != 6 {
		t.Fatalf("历元 12 应唯一剔除 PRN6, got %s/%d", recs[11].Mode, recs[11].ExcludedID)
	}
	// PRN6 在历元 12 刚被唯一剔除的这一刻，不得清零 PRN3 已攒下的计数：
	// 历元 11 streak=1，历元 12 streak=2（不是 0 重来）。
	if it := isoTest(recs, 11, 3); it.NormalStreak != 1 {
		t.Fatalf("历元 11 PRN3 streak=%d, 期望 1", it.NormalStreak)
	}
	if it := isoTest(recs, 12, 3); it.NormalStreak != 2 {
		t.Fatalf("历元 12 PRN6 刚剔除不应清零 PRN3 计数, streak=%d 期望 2", it.NormalStreak)
	}
	if it := isoTest(recs, 13, 3); it.NormalStreak != 3 {
		t.Fatalf("历元 13 PRN3 streak=%d, 期望 3", it.NormalStreak)
	}
	assertIsolated(t, recs[13], 3, false) // 历元 14 解除
	assertIsolated(t, recs[39], 6, true)  // PRN6 偏差仍在，永不放回
	if len(recs[39].SatResults) != 9 {
		t.Fatalf("历元 40 参与解算星数=%d, 期望 9", len(recs[39].SatResults))
	}
}

// TestDualFaultBothRecover 两颗偏差都撤掉（PRN6 历元8..20），两颗都要回得来。
func TestDualFaultBothRecover(t *testing.T) {
	svc := session.NewService([]profile.Profile{isoProfile()})
	sess, _ := svc.NewSession("s", "iso4")
	var recs []*session.EpochRecord
	// PRN6 历元 8..20（1-based）
	for _, e := range dualEpochs(8, 20) {
		r, err := svc.Step(sess, e)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
	assertIsolated(t, recs[13], 3, false) // PRN3 历元 14 回
	// PRN6 偏差历元 20 撤：正常历元 21/22/23（streak 1/2/3），历元 24（streak 4）解除
	for seq := 21; seq <= 23; seq++ {
		if it := isoTest(recs, seq, 6); it.NormalStreak != seq-20 || !it.Pass {
			t.Fatalf("历元 %d PRN6 streak=%d pass=%v", seq, it.NormalStreak, it.Pass)
		}
		assertIsolated(t, recs[seq-1], 6, true)
	}
	assertIsolated(t, recs[23], 6, false) // 0-based 23 = 历元24
	if len(recs[39].SatResults) != 10 {
		t.Fatalf("两颗都回来后历元 40 应 10 颗参与解算, got %d", len(recs[39].SatResults))
	}
	// 同一历元多颗星都满足时各自独立计数、均可解除（本场景两者解除时刻不同，
	// 这里额外构造同刻满足的情况由 statem 层行为保证：见下方同时恢复子用例）。
}

// TestSimultaneousReleaseSameEpoch 两颗隔离星在同一历元都攒够历元时一起放回
// （选型一：同时放回，不在同历元串行复核）。
func TestSimultaneousReleaseSameEpoch(t *testing.T) {
	prof := isoProfile()
	svc := session.NewService([]profile.Profile{prof})
	sess, _ := svc.NewSession("s", "iso4")
	c := dualSky()

	// 构造初始状态：PRN3、PRN6 均已隔离，且各已连续正常 3 个历元（再 1 个即满 4）。
	// 与"可见星 10 颗"的观测配合：活动星为 8 颗。
	if err := sess.SetIsolatedForTest(map[int]int{3: 3, 6: 3}, 10); err != nil {
		t.Fatal(err)
	}

	// 此后数据完全干净：下一历元两颗都应判为正常（第 4 个连续历元）并同时解除
	ep := c.Observe(sim.Obs{
		Rng: rand.New(rand.NewSource(55)), Sigma: 1,
	})
	r, err := svc.Step(sess, toInput(1001, ep))
	if err != nil {
		t.Fatal(err)
	}
	if contains(r.Isolated, 3) || contains(r.Isolated, 6) {
		t.Fatalf("两颗应在同一历元一起解除, isolated=%v", r.Isolated)
	}
	if len(r.IsolationTests) != 0 {
		t.Fatalf("刚解除的星不再出现在隔离检验明细中, got %d 行", len(r.IsolationTests))
	}
	// 解除发生在解算之后：本历元解算仍用 8 颗活动星（PRN3/PRN6 下一历元才回来）
	if len(r.SatResults) != 8 {
		t.Fatalf("解除发生在解算之后，本历元解算仍用 8 颗活动星, got %d", len(r.SatResults))
	}
	// 下一历元两颗一起重新参与解算
	ep2 := c.Observe(sim.Obs{Rng: rand.New(rand.NewSource(56)), Sigma: 1})
	r2, err := svc.Step(sess, toInput(1002, ep2))
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.SatResults) != 10 {
		t.Fatalf("下一历元两颗都应重新参与解算（10 颗）, got %d", len(r2.SatResults))
	}
}

// TestDualFaultBatchEqualsIndividual 两组双星数据：整段批量与逐历元逐项相等。
func TestDualFaultBatchEqualsIndividual(t *testing.T) {
	for _, start := range []int{8, 12} {
		epochs := dualEpochs(start, -1)

		stA := newStore(t)
		if err := stA.CreateProfile(isoProfile()); err != nil {
			t.Fatal(err)
		}
		if _, err := stA.CreateSession("s", "iso4"); err != nil {
			t.Fatal(err)
		}
		recsA, err := stA.AppendBatch("s", epochs)
		if err != nil {
			t.Fatal(err)
		}
		sessA, _ := stA.GetSession("s")

		stB := newStore(t)
		if err := stB.CreateProfile(isoProfile()); err != nil {
			t.Fatal(err)
		}
		if _, err := stB.CreateSession("s", "iso4"); err != nil {
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

		assertRecordsEqual(t, recsA, recsB) // 含 isolation_tests 逐项相等
		assertRecordsEqual(t, sessA.Records, sessB.Records)
		assertStateEqual(t, sessA, sessB)
	}
}

// TestDualFaultRestartResumeMidIsolation 双星都在隔离期间“重启”，分段续跑与一口气相等。
func TestDualFaultRestartResumeMidIsolation(t *testing.T) {
	epochs := dualEpochs(8, 20)
	dir := filepath.Join(t.TempDir(), "data")

	stFull, err := session.NewStore(dir + "_full")
	if err != nil {
		t.Fatal(err)
	}
	if err := stFull.CreateProfile(isoProfile()); err != nil {
		t.Fatal(err)
	}
	if _, err := stFull.CreateSession("s", "iso4"); err != nil {
		t.Fatal(err)
	}
	if _, err := stFull.AppendBatch("s", epochs); err != nil {
		t.Fatal(err)
	}
	sessFull, _ := stFull.GetSession("s")

	// 分段点选在两颗星都隔离、PRN3 正攒恢复计数的历元 12 之后
	const split = 12
	st1, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st1.CreateProfile(isoProfile()); err != nil {
		t.Fatal(err)
	}
	if _, err := st1.CreateSession("s", "iso4"); err != nil {
		t.Fatal(err)
	}
	for _, e := range epochs[:split] {
		if _, err := st1.AppendEpoch("s", e); err != nil {
			t.Fatal(err)
		}
	}
	st2, err := session.NewStore(dir) // “重启”
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
	// PRN3 仍应在历元 14（split 之后第 2 个历元）解除
	if contains(sessResumed.Records[13].Isolated, 3) {
		t.Fatal("重启续跑后 PRN3 仍应在历元 14 解除")
	}
}

// TestPreUpgradeSessionResume 从升级前写出的会话文件接着提交：
// 能加载、能继续；旧记录原样不改写（含不含新字段的字节级事实）；
// 续跑结果与一口气跑完一致。
func TestPreUpgradeSessionResume(t *testing.T) {
	src := filepath.Join("testdata", "upgrade")
	dir := filepath.Join(t.TempDir(), "upgrade")
	if err := copyDir(src, dir); err != nil {
		t.Fatal(err)
	}

	// 升级前会话文件的原始字节（用于断言前 12 条记录不被改写）
	rawBefore, err := os.ReadFile(filepath.Join(dir, "sessions", "dualfault.json"))
	if err != nil {
		t.Fatal(err)
	}

	st, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.GetSession("dualfault")
	if err != nil {
		t.Fatalf("升级前会话应能加载: %v", err)
	}
	if sess.Stats.Epochs != 12 {
		t.Fatalf("应已有 12 条历史记录, got %d", sess.Stats.Epochs)
	}
	if len(sess.State.Isolated) != 2 ||
		sess.State.Isolated[3].NormalStreak != 2 ||
		sess.State.Isolated[6].NormalStreak != 0 {
		t.Fatalf("升级前隔离状态加载错误: %+v", sess.State.Isolated)
	}
	// 旧记录没有新字段
	for i, r := range sess.Records {
		if len(r.IsolationTests) != 0 {
			t.Fatalf("旧记录 %d 不应被凭空填充新字段", i)
		}
	}

	// 用一口气跑完的结果作为对照（新会话、同一运行档）
	fullStore := newStore(t)
	if err := fullStore.CreateProfile(isoProfileWithName("iso4-upgrade")); err != nil {
		t.Fatal(err)
	}
	if _, err := fullStore.CreateSession("full", "iso4-upgrade"); err != nil {
		t.Fatal(err)
	}
	fullRecs, err := fullStore.AppendBatch("full", dualEpochs(8, -1))
	if err != nil {
		t.Fatal(err)
	}

	// 从历元 13 起逐历元续跑
	for _, e := range dualEpochs(8, -1)[12:] {
		if _, err := st.AppendEpoch("dualfault", e); err != nil {
			t.Fatal(err)
		}
	}
	resumed, err := st.GetSession("dualfault")
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Records) != 40 {
		t.Fatalf("续跑后应有 40 条记录, got %d", len(resumed.Records))
	}
	// 前 12 条：与升级前文件中的记录逐字段相等（旧记录无 isolation_tests，
	// Go 零值为 nil；新跑记录无隔离星时为空切片或 nil，JSON 下均省略，故归一化比较）
	for i := 0; i < 12; i++ {
		if !recordsEqualIgnoreEmptyIso(resumed.Records[i], extractRecord(t, rawBefore, i)) {
			t.Fatalf("历史记录 %d 被改写:\n new=%+v\n old=%+v",
				i+1, resumed.Records[i], extractRecord(t, rawBefore, i))
		}
		if len(resumed.Records[i].IsolationTests) != 0 {
			t.Fatalf("旧记录 %d 不应出现 isolation_tests", i+1)
		}
	}
	// 续跑新产生的历元 13..40 与一口气跑完的对应记录逐项相等
	// （含新字段 isolation_tests；前 12 条是升级前历史记录，按上面的规则单独校验）。
	assertRecordsEqual(t, resumed.Records[12:], fullRecs[12:])
	assertIsolated(t, resumed.Records[13], 3, false) // PRN3 历元14 解除
	assertIsolated(t, resumed.Records[39], 6, true)  // PRN6 永不放回

	// 落盘文件里前 12 条 JSON 文本与升级前完全一致（字节级不改写）
	rawAfter, err := os.ReadFile(filepath.Join(dir, "sessions", "dualfault.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !jsonPrefixRecordsUnchanged(t, rawBefore, rawAfter, 12) {
		t.Fatal("落盘文件中升级前 12 条记录的 JSON 文本被改写")
	}

	// 第二个夹具：单星隔离中的升级前会话也能续跑
	sess2, err := st.GetSession("singlefault")
	if err != nil {
		t.Fatal(err)
	}
	if sess2.Stats.Epochs != 12 || len(sess2.State.Isolated) != 1 {
		t.Fatalf("singlefault 夹具状态异常: %+v", sess2.State.Isolated)
	}
}

// recordsEqualIgnoreEmptyIso 比较两条记录，允许 isolation_tests 在“空（nil/[]）”
// 意义下等价（旧记录 JSON 省略该键，新记录无隔离星时 omitempty 同样省略）。
// 若任一方存在非空明细则仍要求逐项相等。
func recordsEqualIgnoreEmptyIso(a, b *session.EpochRecord) bool {
	if len(a.IsolationTests) != 0 || len(b.IsolationTests) != 0 {
		return reflect.DeepEqual(a, b)
	}
	cpA, cpB := *a, *b
	cpA.IsolationTests, cpB.IsolationTests = nil, nil
	return reflect.DeepEqual(&cpA, &cpB)
}

// assertRecordsEqualIsoNil 同 assertRecordsEqual，但把“空明细”的 nil/[] 视为等价
// （旧记录 JSON 省略 isolation_tests，新记录无隔离星时 omitempty 亦省略）。
func assertRecordsEqualIsoNil(t *testing.T, a, b []*session.EpochRecord) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("历元数不同 %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !recordsEqualIgnoreEmptyIso(a[i], b[i]) {
			t.Fatalf("历元 %d (ts=%d) 记录不一致:\n A=%+v\n B=%+v", i, a[i].Timestamp, a[i], b[i])
		}
	}
}

// ---- helpers ----

func isoProfileWithName(name string) profile.Profile {
	p := isoProfile()
	p.Name = name
	return p
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode())
	})
}

// extractRecord 从升级前原始会话 JSON 中按序取出第 i 条记录（反序列化为新结构，
// 新字段缺省为零值——旧记录本就不该有这些字段）。
func extractRecord(t *testing.T, raw []byte, i int) *session.EpochRecord {
	t.Helper()
	var old struct {
		Records []*session.EpochRecord `json:"records"`
	}
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	return old.Records[i]
}

// jsonPrefixRecordsUnchanged 比较两个会话 JSON 中前 n 条 records 的紧凑编码是否一致。
func jsonPrefixRecordsUnchanged(t *testing.T, before, after []byte, n int) bool {
	t.Helper()
	var a, b struct {
		Records []json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(before, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &b); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if compact(a.Records[i]) != compact(b.Records[i]) {
			t.Fatalf("第 %d 条记录 JSON 被改写:\n before=%s\n after =%s",
				i+1, a.Records[i], b.Records[i])
		}
	}
	return true
}

func compact(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

var _ = sort.Ints
var _ = math.Abs
