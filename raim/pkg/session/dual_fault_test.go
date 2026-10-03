package session_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"raim/internal/sim"
	"raim/pkg/profile"
	"raim/pkg/session"
)

// dualFaultProf 与试飞组自建运行档一致：隔离恢复需 4 个历元、告警限很大。
func dualFaultProf() profile.Profile {
	return profile.Profile{
		Name: "iso4", Pfa: 1e-3, Pmd: 1e-3, HAL: 1e9,
		Isolation: profile.Isolation{MinEpochs: 4},
		Alert: profile.Alert{
			Mode: profile.Snapshot, ConfirmEpochs: 1, ClearEpochs: 1,
		},
	}
}

// dualSky：10 颗星，σ=1m，固定几何与噪声，保证两组数据可稳定复现。
func dualSky(t *testing.T) (*sim.Constellation, []session.EpochInput, func(int) map[int]float64) {
	t.Helper()
	c := sim.EvenSky(sim.DefaultReceiver(), 10, 10, 42)
	bias3to10 := func(i int) map[int]float64 {
		// PRN3：第 5~10 历元（0-based 4..9）+80m；PRN6：第 8 历元起（i>=7）一直 +80m
		b := map[int]float64{}
		if i >= 4 && i <= 9 {
			b[3] = 80
		}
		if i >= 7 {
			b[6] = 80
		}
		if len(b) == 0 {
			return nil
		}
		return b
	}
	epochs := genEpochs(c, 40, 1, 20261003, bias3to10)
	return c, epochs, bias3to10
}

func runDual(t *testing.T, epochs []session.EpochInput) (*session.Session, []*session.EpochRecord) {
	t.Helper()
	prof := dualFaultProf()
	svc := session.NewService([]profile.Profile{prof})
	sess, err := svc.NewSession("s", "iso4")
	if err != nil {
		t.Fatal(err)
	}
	recs := make([]*session.EpochRecord, 0, len(epochs))
	for _, e := range epochs {
		r, err := svc.Step(sess, e)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
	return sess, recs
}

func TestDualFaultExclusionEpochs(t *testing.T) {
	_, epochs, _ := dualSky(t)
	_, recs := runDual(t, epochs)
	if recs[4].Mode != "excluded" || recs[4].ExcludedID != 3 {
		t.Fatalf("第5历元应唯一剔除 PRN3, got mode=%s excluded=%d", recs[4].Mode, recs[4].ExcludedID)
	}
	if recs[7].Mode != "excluded" || recs[7].ExcludedID != 6 {
		t.Fatalf("第8历元应唯一剔除 PRN6, got mode=%s excluded=%d", recs[7].Mode, recs[7].ExcludedID)
	}
}

func TestDualFaultPRN3RecoversAt14(t *testing.T) {
	_, epochs, _ := dualSky(t)
	_, recs := runDual(t, epochs)

	// 偏差 10 撤掉，11 起攒恢复历元：11/12/13 streak=1/2/3 仍隔离，14 解除。
	wantStreak := map[int]int{10: 0, 11: 1, 12: 2, 13: 3}
	for epoch1, streak := range wantStreak {
		ch := findCheck(recs[epoch1-1], 3)
		if ch == nil {
			t.Fatalf("第%d历元缺少 PRN3 的隔离检验输出", epoch1)
		}
		if ch.NormalStreak != streak {
			t.Fatalf("第%d历元 PRN3 normal_streak=%d, want %d (check=%+v)",
				epoch1, ch.NormalStreak, streak, ch)
		}
	}
	// 11 起每个历元单星检验与加回检验都应通过（PRN6 的坏残差不在它的基线里）
	for i := 10; i <= 13; i++ {
		ch := findCheck(recs[i], 3)
		if !ch.Passed || ch.TestStat > ch.SelfThreshold || ch.AddSSE > ch.AddThreshold {
			t.Fatalf("第%d历元 PRN3 应通过恢复检验: %+v", i+1, ch)
		}
	}
	if !contains(recs[12].Isolated, 3) {
		t.Fatal("第13历元 PRN3 计数未满，应仍隔离")
	}
	if contains(recs[13].Isolated, 3) {
		t.Fatalf("第14历元 PRN3 应解除隔离, isolated=%v", recs[13].Isolated)
	}
	// 解除发生在解算之后：第15历元才重新参与解算，活动星恢复为 9
	if n := len(recs[13].SatResults); n != 8 {
		t.Fatalf("第14历元解算星数=%d, want 8（解除在下一历元生效）", n)
	}
	if n := len(recs[14].SatResults); n != 9 {
		t.Fatalf("第15历元解算星数=%d, want 9（PRN3 回解，PRN6 仍隔离）", n)
	}
	// PRN6 偏差仍在：一直隔离到第40历元，恢复检验每个历元都不过
	for i := 7; i < 40; i++ {
		ch := findCheck(recs[i], 6)
		if ch == nil {
			t.Fatalf("第%d历元缺少 PRN6 的隔离检验输出", i+1)
		}
		if i >= 8 && ch.Passed {
			t.Fatalf("第%d历元 PRN6 偏差仍在，不应通过恢复检验: %+v", i+1, ch)
		}
		if !contains(recs[i].Isolated, 6) {
			t.Fatalf("第%d历元 PRN6 应仍隔离", i+1)
		}
	}
}

// TestDualFaultNewExclusionKeepsStreak：PRN6 从第 12 历元才开始加偏差，
// 即 PRN3 正在攒恢复历元时 PRN6 被唯一剔除——该历元不能清掉 PRN3 的计数，
// PRN3 仍应在第 14 历元解除。
func TestDualFaultNewExclusionKeepsStreak(t *testing.T) {
	c := sim.EvenSky(sim.DefaultReceiver(), 10, 10, 42)
	bias := func(i int) map[int]float64 {
		b := map[int]float64{}
		if i >= 4 && i <= 9 { // PRN3 第5~10历元
			b[3] = 80
		}
		if i >= 11 { // PRN6 第12历元起
			b[6] = 80
		}
		if len(b) == 0 {
			return nil
		}
		return b
	}
	epochs := genEpochs(c, 40, 1, 20261003, bias)
	_, recs := runDual(t, epochs)

	if recs[11].Mode != "excluded" || recs[11].ExcludedID != 6 {
		t.Fatalf("第12历元应剔除 PRN6, got mode=%s excluded=%d", recs[11].Mode, recs[11].ExcludedID)
	}
	// PRN3：11 streak1、12（PRN6 被剔的历元）streak2，未被清零
	for epoch1, want := range map[int]int{11: 1, 12: 2, 13: 3} {
		ch := findCheck(recs[epoch1-1], 3)
		if ch == nil || ch.NormalStreak != want {
			t.Fatalf("第%d历元 PRN3 streak 异常: %+v", epoch1, ch)
		}
	}
	if contains(recs[13].Isolated, 3) {
		t.Fatal("PRN3 应在第14历元解除，即使第12历元 PRN6 刚被剔")
	}
	if !contains(recs[13].Isolated, 6) {
		t.Fatal("第14历元 PRN6 应仍隔离")
	}
}

// TestDualFaultBothRecoverAfterBiasOff：两颗偏差都撤掉后，两颗都要能回到解算。
func TestDualFaultBothRecoverAfterBiasOff(t *testing.T) {
	c := sim.EvenSky(sim.DefaultReceiver(), 10, 10, 42)
	bias := func(i int) map[int]float64 {
		b := map[int]float64{}
		if i >= 4 && i <= 9 { // PRN3：5~10
			b[3] = 80
		}
		if i >= 7 && i <= 19 { // PRN6：8~20
			b[6] = 80
		}
		if len(b) == 0 {
			return nil
		}
		return b
	}
	epochs := genEpochs(c, 40, 1, 20261003, bias)
	_, recs := runDual(t, epochs)

	// PRN3 第14历元（index 13）解除
	if contains(recs[13].Isolated, 3) {
		t.Fatal("PRN3 应在第14历元解除")
	}
	// PRN6：偏差到第20历元（index 19）。第20历元仍有偏差、检验不过；
	// 21 起攒 4 个正常历元，第24历元（index 23）解除。
	for i := 20; i <= 23; i++ {
		ch := findCheck(recs[i], 6)
		if ch == nil || !ch.Passed {
			t.Fatalf("第%d历元 PRN6 恢复检验应通过: %+v", i+1, ch)
		}
	}
	if !contains(recs[22].Isolated, 6) {
		t.Fatal("第23历元 PRN6 计数未满应仍隔离")
	}
	if contains(recs[23].Isolated, 6) {
		t.Fatal("第24历元 PRN6 应解除隔离")
	}
	// 第25历元起两颗都回到解算：10 颗星
	if n := len(recs[24].SatResults); n != 10 {
		t.Fatalf("第25历元解算星数=%d, want 10", n)
	}
	if len(recs[39].Isolated) != 0 {
		t.Fatalf("第40历元不应有隔离星, got %v", recs[39].Isolated)
	}
}

// TestIsolationChecksPresentEveryEpoch：每个历元每颗可见隔离星都必须带
// 检验量、门限、过没过、连续正常历元数；新被剔除的星当历元也要有。
func TestIsolationChecksPresentEveryEpoch(t *testing.T) {
	_, epochs, _ := dualSky(t)
	_, recs := runDual(t, epochs)

	for i, r := range recs {
		wantIDs := append([]int{}, r.Isolated...)
		sort.Ints(wantIDs)
		var gotIDs []int
		for _, ch := range r.IsolationChecks {
			gotIDs = append(gotIDs, ch.ID)
			if ch.AddThreshold <= 0 || ch.SelfThreshold <= 0 {
				t.Fatalf("第%d历元星%d门限异常: %+v", i+1, ch.ID, ch)
			}
			if ch.Passed != (ch.TestStat <= ch.SelfThreshold && ch.AddSSE <= ch.AddThreshold) {
				t.Fatalf("第%d历元星%d passed 与检验量不一致: %+v", i+1, ch.ID, ch)
			}
			if ch.Passed && ch.NormalStreak == 0 {
				t.Fatalf("第%d历元星%d通过但 streak=0: %+v", i+1, ch.ID, ch)
			}
		}
		sort.Ints(gotIDs)
		// 本历元仍隔离的星必须都有检验快照；另外允许出现“本历元刚放回”的星
		// （它决定放回的最后一次检验，streak 已达标）。
		for _, want := range wantIDs {
			found := false
			for _, g := range gotIDs {
				if g == want {
					found = true
				}
			}
			if !found {
				t.Fatalf("第%d历元隔离星 %d 缺少检验快照, checks=%v", i+1, want, gotIDs)
			}
		}
		checkByID := map[int]session.IsolationCheck{}
		for _, ch := range r.IsolationChecks {
			checkByID[ch.ID] = ch
		}
		for _, g := range gotIDs {
			ch := checkByID[g]
			if contains(r.Isolated, g) {
				continue
			}
			// 已不在隔离列表 => 只能是本历元刚放回：必须通过且 streak 达标
			if !ch.Passed || ch.NormalStreak < 4 {
				t.Fatalf("第%d历元星%d已不在隔离列表但检验不达标: %+v", i+1, g, ch)
			}
		}
	}
	// 第5历元：PRN3 当历元刚被剔除，也要出现且 streak=0
	ch := findCheck(recs[4], 3)
	if ch == nil || ch.NormalStreak != 0 || ch.Passed {
		t.Fatalf("第5历元 PRN3 检验快照异常: %+v", ch)
	}
}

// TestDualFaultBothQualifySameEpoch：两颗在同一历元攒满恢复计数。
// PRN3 第5历元起、PRN6 第6历元起先后被唯一剔除；两颗偏差都到第9历元，
// 第10历元起各自在干净基线上攒恢复历元，第13历元（index 12）同时攒满 4 个。
func TestDualFaultBothQualifySameEpoch(t *testing.T) {
	c := sim.EvenSky(sim.DefaultReceiver(), 10, 10, 42)
	bias := func(i int) map[int]float64 {
		b := map[int]float64{}
		if i >= 4 && i <= 8 { // PRN3：第5~9历元
			b[3] = 80
		}
		if i >= 5 && i <= 8 { // PRN6：第6~9历元
			b[6] = 80
		}
		if len(b) == 0 {
			return nil
		}
		return b
	}
	epochs := genEpochs(c, 40, 1, 20261003, bias)
	_, recs := runDual(t, epochs)

	// 第5历元：仅 PRN3 带偏差，唯一排除
	if recs[4].Mode != "excluded" || recs[4].ExcludedID != 3 {
		t.Fatalf("第5历元应排除 PRN3, got %s/%d", recs[4].Mode, recs[4].ExcludedID)
	}
	// 第6历元：PRN3 已隔离，活动集里只剩 PRN6 带偏差，唯一排除
	if recs[5].Mode != "excluded" || recs[5].ExcludedID != 6 {
		t.Fatalf("第6历元应排除 PRN6, got %s/%d", recs[5].Mode, recs[5].ExcludedID)
	}
	// 第13历元（index 12）：两颗同时攒满 4 个恢复历元
	for _, id := range []int{3, 6} {
		ch := findCheck(recs[12], id)
		if ch == nil || !ch.Passed || ch.NormalStreak != 4 {
			t.Fatalf("第13历元 PRN%d 应通过且 streak=4: %+v", id, ch)
		}
	}
	if len(recs[12].Isolated) != 0 {
		t.Fatalf("第13历元两颗都应解除, isolated=%v", recs[12].Isolated)
	}
	// 下一历元两颗都回到解算
	if n := len(recs[13].SatResults); n != 10 {
		t.Fatalf("第14历元两颗都应回解, 星数=%d want 10", n)
	}
}

func findCheck(r *session.EpochRecord, id int) *session.IsolationCheck {
	for i := range r.IsolationChecks {
		if r.IsolationChecks[i].ID == id {
			return &r.IsolationChecks[i]
		}
	}
	return nil
}

var _ = sort.Ints

// ---- 批量/逐历元一致性、重启续跑（双星隔离期间） ----

func TestDualFaultBatchEqualsIndividual(t *testing.T) {
	_, epochs, _ := dualSky(t)

	stA := newStore(t)
	if err := stA.CreateProfile(dualFaultProf()); err != nil {
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
	if err := stB.CreateProfile(dualFaultProf()); err != nil {
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

	assertRecordsEqual(t, recsA, recsB)
	assertRecordsEqual(t, sessA.Records, sessB.Records)
	assertStateEqual(t, sessA, sessB)
}

// TestDualFaultRestartMidIsolation：在双星都处于隔离时（第10历元后）
// “重启”，换 Store 实例从磁盘续跑，结果与一口气跑完逐项相等。
func TestDualFaultRestartMidIsolation(t *testing.T) {
	_, epochs, _ := dualSky(t)
	dir := filepath.Join(t.TempDir(), "data")

	stFull, err := session.NewStore(dir + "_full")
	if err != nil {
		t.Fatal(err)
	}
	if err := stFull.CreateProfile(dualFaultProf()); err != nil {
		t.Fatal(err)
	}
	if _, err := stFull.CreateSession("s", "iso4"); err != nil {
		t.Fatal(err)
	}
	if _, err := stFull.AppendBatch("s", epochs); err != nil {
		t.Fatal(err)
	}
	sessFull, _ := stFull.GetSession("s")

	// 分段：先送 10 个历元（此时 PRN3、PRN6 都在隔离），再重启续跑
	st1, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st1.CreateProfile(dualFaultProf()); err != nil {
		t.Fatal(err)
	}
	if _, err := st1.CreateSession("s", "iso4"); err != nil {
		t.Fatal(err)
	}
	for _, e := range epochs[:10] {
		if _, err := st1.AppendEpoch("s", e); err != nil {
			t.Fatal(err)
		}
	}
	mid, _ := st1.GetSession("s")
	if got := sortedIsolatedIDs(mid); !reflect.DeepEqual(got, []int{3, 6}) {
		t.Fatalf("重启前隔离集合=%v, want [3 6]", got)
	}

	st2, err := session.NewStore(dir) // 重新打开目录 = 重启
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range epochs[10:] {
		if _, err := st2.AppendEpoch("s", e); err != nil {
			t.Fatal(err)
		}
	}
	sessResumed, _ := st2.GetSession("s")

	assertRecordsEqual(t, sessResumed.Records, sessFull.Records)
	assertStateEqual(t, sessResumed, sessFull)
}

// ---- 升级前会话兼容 ----

// TestLegacySessionResume：数据目录里放一个“升级前”写出的会话文件
// （没有 isolation_checks 字段、旧结构），其中正有星在隔离；
// 升级后必须能接着提交，且已有记录原样保留、不被改写。
func TestLegacySessionResume(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	st, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProfile(dualFaultProf()); err != nil {
		t.Fatal(err)
	}

	c := sim.EvenSky(sim.DefaultReceiver(), 10, 10, 42)
	const legacyN = 9 // 送到第9历元：PRN3、PRN6 都在隔离
	bias := func(i int) map[int]float64 {
		b := map[int]float64{}
		if i >= 4 && i <= 9 {
			b[3] = 80
		}
		if i >= 7 {
			b[6] = 80
		}
		if len(b) == 0 {
			return nil
		}
		return b
	}
	legacyEpochs := genEpochs(c, legacyN, 1, 20261003, bias)

	// 1) 用当前代码先跑出前 9 个历元的状态
	svc := session.NewService([]profile.Profile{dualFaultProf()})
	sess, err := svc.NewSession("legacy", "iso4")
	if err != nil {
		t.Fatal(err)
	}
	var legacyRecs []*session.EpochRecord
	for _, e := range legacyEpochs {
		r, err := svc.Step(sess, e)
		if err != nil {
			t.Fatal(err)
		}
		legacyRecs = append(legacyRecs, r)
	}
	// 2) 剥掉升级后新增的输出字段，模拟升级前落盘内容
	oldRecs := make([]map[string]json.RawMessage, 0, len(sess.Records))
	for _, r := range sess.Records {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		// omitempty 下新字段本来就不会出现；显式删除以防语义变化
		delete(m, "isolation_checks")
		oldRecs = append(oldRecs, m)
	}
	legacyJSON := struct {
		ID            string                       `json:"id"`
		ProfileName   string                       `json:"profile_name"`
		LastTS        *int64                       `json:"last_ts"`
		State         map[string]any               `json:"state"`
		Stats         session.Stats                `json:"stats"`
		Records       []map[string]json.RawMessage `json:"records"`
		AlertOpenRisk bool                         `json:"alert_open_risk"`
	}{
		ID:          "legacy",
		ProfileName: "iso4",
		LastTS:      sess.LastTS,
		State: map[string]any{
			"isolated": map[string]any{
				"3": map[string]any{"id": 3, "normal_streak": 0, "since_epoch_seq": 5},
				"6": map[string]any{"id": 6, "normal_streak": 0, "since_epoch_seq": 8},
			},
			"alert": map[string]any{"active": false, "bad_streak": 0, "good_streak": 1},
		},
		Stats:         sess.Stats,
		Records:       oldRecs,
		AlertOpenRisk: false,
	}
	// 直接写入 sessions 目录，模拟升级前就存在的文件
	sessPath := filepath.Join(dir, "sessions", "legacy.json")
	if err := writeAtomicJSON(sessPath, legacyJSON); err != nil {
		t.Fatal(err)
	}
	rawBefore, err := os.ReadFile(sessPath)
	if err != nil {
		t.Fatal(err)
	}

	// 3) 升级后打开旧会话并续提第 10 个历元
	st2, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st2.GetSession("legacy")
	if err != nil {
		t.Fatalf("读取升级前会话失败: %v", err)
	}
	if ids := sortedIsolatedIDs(got); !reflect.DeepEqual(ids, []int{3, 6}) {
		t.Fatalf("旧会话隔离状态加载异常: %v", ids)
	}
	if got.Stats.Epochs != legacyN {
		t.Fatalf("旧会话历元数=%d, want %d", got.Stats.Epochs, legacyN)
	}
	next := genEpochs(c, 40, 1, 20261003, bias)
	r10, err := st2.AppendEpoch("legacy", next[legacyN])
	if err != nil {
		t.Fatalf("旧会话续提失败: %v", err)
	}
	// 续跑历元应带新输出，两颗隔离星都在
	if len(r10.IsolationChecks) != 2 {
		t.Fatalf("续跑历元隔离检验数=%d, want 2", len(r10.IsolationChecks))
	}

	// 4) 已有 9 条记录保持原样（字段值不变；旧记录没有 isolation_checks）
	resumed, _ := st2.GetSession("legacy")
	if len(resumed.Records) != legacyN+1 {
		t.Fatalf("续跑后记录数=%d, want %d", len(resumed.Records), legacyN+1)
	}
	for i := 0; i < legacyN; i++ {
		a, b := resumed.Records[i], legacyRecs[i]
		if a.Seq != b.Seq || a.Mode != b.Mode || a.ExcludedID != b.ExcludedID ||
			a.SSE != b.SSE || a.HPL != b.HPL || a.Timestamp != b.Timestamp ||
			!reflect.DeepEqual(a.Isolated, b.Isolated) || a.Alert != b.Alert {
			t.Fatalf("旧记录 %d 被改写:\n old=%+v\n new=%+v", i+1, b, a)
		}
		if len(a.IsolationChecks) != 0 {
			t.Fatalf("旧记录 %d 不应被回填新字段", i+1)
		}
	}
	// 5) 文件中旧记录的原始字节段仍在（未整体重写旧内容之外的语义至少保持兼容）
	rawAfter, err := os.ReadFile(sessPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = rawBefore
	if len(rawAfter) <= len(rawBefore) {
		t.Fatal("续跑后会话文件应只追加、不应截断旧内容")
	}

	// 6) 一路续跑到第40历元：PRN3 第14历元解除等行为与新会话一致
	for _, e := range next[legacyN+1:] {
		if _, err := st2.AppendEpoch("legacy", e); err != nil {
			t.Fatal(err)
		}
	}
	final, _ := st2.GetSession("legacy")
	if contains(final.Records[13].Isolated, 3) {
		t.Fatal("旧会话续跑后 PRN3 也应在第14历元解除")
	}
	if !contains(final.Records[39].Isolated, 6) {
		t.Fatal("旧会话续跑后 PRN6 应仍隔离到第40历元")
	}
}

func writeAtomicJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
