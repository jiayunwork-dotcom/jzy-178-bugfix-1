package detect_test

import (
	"math"
	"math/rand"
	"testing"

	"raim/internal/sim"
	"raim/pkg/detect"
	"raim/pkg/geo"
)

func opts() detect.Options { return detect.Options{Pfa: 1e-5} }

func TestNoFaultBelowThreshold(t *testing.T) {
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 200; i++ {
		a, err := detect.Assess(c.Observe(sim.Obs{Rng: rng}), opts())
		if err != nil {
			t.Fatal(err)
		}
		if a.Mode == detect.ModeDetected {
			t.Fatalf("无故障历元 %d 检出: SSE=%.2f thr=%.2f", i, a.SSE, a.Threshold)
		}
		if a.SSE > a.Threshold {
			t.Fatal("Mode 与 SSE/阈值矛盾")
		}
	}
}

func TestBiasDetectedAndCorrectSatelliteExcluded(t *testing.T) {
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)
	const badID = 4
	a, err := detect.Assess(c.Observe(sim.Obs{
		Rng:  rand.New(rand.NewSource(2)),
		Bias: map[int]float64{badID: 60},
	}), opts())
	if err != nil {
		t.Fatal(err)
	}
	if a.Mode != detect.ModeExcluded || a.ExcludedID != badID {
		t.Fatalf("模式=%v 排除=%v, 期望排除 #%d；trials=%+v",
			a.Mode, a.ExcludedID, badID, a.Trials)
	}
	// 剔后定位回到真值附近：60m 偏差已去除，只剩 σ=1m 的噪声（<10m）
	if d := geo.Norm(geo.Sub(a.Sol.Pos, rec.Pos)); d > 10 {
		t.Fatalf("剔后定位偏差 %.3f m", d)
	}
	// 对照：若剔的是另一颗星，故障偏差仍在，定位明显偏离
	for _, tr := range a.Trials {
		if tr.ExcludedID != badID {
			// 通过检验的 trial 只应有坏星那颗；其余 SSE 都应超阈值
			if tr.Passes {
				t.Fatalf("好星 #%d 被剔后竟通过检验", tr.ExcludedID)
			}
		}
	}
}

func TestSSEMonotonic(t *testing.T) {
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)
	prev := -1.0
	for _, b := range []float64{0, 15, 30, 60, 120} {
		a, err := detect.Assess(c.Observe(sim.Obs{
			Rng:  rand.New(rand.NewSource(2)),
			Bias: map[int]float64{4: b},
		}), opts())
		if err != nil {
			t.Fatal(err)
		}
		if a.SSE <= prev {
			t.Fatalf("b=%v SSE=%v 不单调", b, a.SSE)
		}
		prev = a.SSE
	}
}

func TestFourSatsUnavailable(t *testing.T) {
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 4, 20, 1)
	a, err := detect.Assess(c.Observe(sim.Obs{}), opts())
	if err != nil {
		t.Fatal(err)
	}
	if a.Mode != detect.ModeUnavailable {
		t.Fatalf("4 星模式=%v", a.Mode)
	}
}

func TestFiveSatsDetectButCannotExclude(t *testing.T) {
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 5, 20, 1)
	a, err := detect.Assess(c.Observe(sim.Obs{
		Rng:  rand.New(rand.NewSource(1)),
		Bias: map[int]float64{1: 80},
	}), opts())
	if err != nil {
		t.Fatal(err)
	}
	if a.Mode != detect.ModeDetected {
		t.Fatalf("5 星大偏差应检出无法排除, got %v", a.Mode)
	}
}

func TestThresholdGrowsAsPfaShrinks(t *testing.T) {
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)
	var thr []float64
	for _, pfa := range []float64{1e-3, 1e-5, 1e-7} {
		a, _ := detect.Assess(c.Observe(sim.Obs{}), detect.Options{Pfa: pfa})
		thr = append(thr, a.Threshold)
	}
	if !(thr[0] < thr[1] && thr[1] < thr[2]) {
		t.Fatalf("阈值未随 Pfa 减小而增大: %v", thr)
	}
}

func TestConstantShiftDoesNotDetect(t *testing.T) {
	// 整体加常数被钟差吸收：不应检出
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)
	clock := 50_000.0
	a, err := detect.Assess(c.Observe(sim.Obs{
		Rng: rand.New(rand.NewSource(3)), Clock: &clock,
	}), opts())
	if err != nil {
		t.Fatal(err)
	}
	if a.Mode == detect.ModeDetected {
		t.Fatal("整体钟差不应被判为故障")
	}
	if math.Abs(a.Sol.ClockBias-50_000) > 3 {
		t.Fatalf("钟差 %v", a.Sol.ClockBias)
	}
}
