package session_test

import (
	"math/rand"
	"testing"

	"raim/internal/sim"
	"raim/pkg/profile"
	"raim/pkg/session"
)

// TestIsolatedThenTooFewActive：多颗星进入隔离后，若某历元可见活动星不足 4 颗，
// 结果应为 unavailable、逐颗可见隔离星仍带检验快照（不过、计数清零）、
// 不可见隔离星不出现在输出中，且按检测失败告警。
func TestIsolatedThenTooFewActive(t *testing.T) {
	prof := profile.Profile{
		Name: "iso4u", Pfa: 1e-3, Pmd: 1e-3, HAL: 1e9,
		Isolation: profile.Isolation{MinEpochs: 4},
		Alert:     profile.Alert{Mode: profile.Snapshot, ConfirmEpochs: 1, ClearEpochs: 1},
	}
	svc := session.NewService([]profile.Profile{prof})
	sess, _ := svc.NewSession("s", "iso4u")
	c := sim.EvenSky(sim.DefaultReceiver(), 10, 10, 42)

	rng := rand.New(rand.NewSource(99))
	step := func(ts int64, hidden []int, bias map[int]float64) *session.EpochRecord {
		ep := c.Observe(sim.Obs{Rng: rng, Sigma: 1.0, Bias: bias, Hidden: hidden})
		r, err := svc.Step(sess, toInput(ts, ep))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// 连续 3 个历元，每历元给一颗尚未隔离的星加 80m 偏差，把它唯一剔除。
	for i, badID := range []int{1, 2, 3} {
		ts := int64(i + 1)
		r := step(ts, nil, map[int]float64{badID: 80})
		if r.Mode != "excluded" || r.ExcludedID != badID {
			t.Fatalf("第%d历元应唯一排除 PRN%d, got mode=%s excluded=%d isolated=%v",
				ts, badID, r.Mode, r.ExcludedID, r.Isolated)
		}
	}
	if n := len(sess.State.Isolated); n != 3 {
		t.Fatalf("应有 3 颗隔离星, got %d", n)
	}

	// 下一历元：活动星本有 7 颗（4..10），隐藏其中 4 颗（4,5,6,7）
	// 只剩 3 颗活动 < 4；同时隐藏隔离星 3（本历元不可见，不应出现在输出中）。
	r := step(4, []int{7, 6, 5, 4, 3}, map[int]float64{})
	if r.Mode != "unavailable" {
		t.Fatalf("活动星不足 4 颗应 unavailable, got %s", r.Mode)
	}
	if !r.Alert {
		t.Fatal("活动星不足应按检测失败告警")
	}
	// 可见隔离星为 1、2（3 被隐藏）
	if len(r.IsolationChecks) != 2 {
		t.Fatalf("可见隔离星检验数=%d, want 2: %+v", len(r.IsolationChecks), r.IsolationChecks)
	}
	for _, ch := range r.IsolationChecks {
		if ch.ID == 3 {
			t.Fatal("不可见隔离星 3 不应出现在检验输出")
		}
		if ch.Passed || ch.NormalStreak != 0 {
			t.Fatalf("无法定位时隔离星 %d 应不过且计数清零: %+v", ch.ID, ch)
		}
	}

	// 下一历元恢复全部可见且无偏差：定位恢复，隔离星重新开始攒计数。
	r2 := step(5, nil, nil)
	if r2.Mode == "unavailable" {
		t.Fatal("星数恢复后不应仍 unavailable")
	}
	if len(r2.IsolationChecks) != 3 {
		t.Fatalf("三星重新可见后检验数=%d, want 3", len(r2.IsolationChecks))
	}
}
