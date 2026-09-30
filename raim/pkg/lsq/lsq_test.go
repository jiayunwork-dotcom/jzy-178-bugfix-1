package lsq_test

import (
	"math"
	"math/rand"
	"testing"

	"raim/internal/sim"
	"raim/pkg/apierr"
	"raim/pkg/geo"
	"raim/pkg/lsq"
)

func TestSolveNoiselessPosition(t *testing.T) {
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 42)
	ep := c.Observe(sim.Obs{})
	sol, err := lsq.Solve(ep)
	if err != nil {
		t.Fatal(err)
	}
	if d := geo.Norm(geo.Sub(sol.Pos, rec.Pos)); d > 0.01 {
		t.Fatalf("无噪声定位偏差 %.4f m", d)
	}
	if math.Abs(sol.ClockBias-rec.ClockBias) > 0.01 {
		t.Fatalf("钟差偏差 %.4f m", sol.ClockBias)
	}
	if !sol.Converged || sol.Iter > 10 {
		t.Fatalf("未正常收敛: iter=%d converged=%v", sol.Iter, sol.Converged)
	}
}

func TestConstantShiftOnlyClock(t *testing.T) {
	// 所有伪距同加 30 km：位置不变、钟差 +30 km
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 7)
	base := c.Observe(sim.Obs{})
	shift := 30_000.0
	clock := rec.ClockBias + shift
	shifted := c.Observe(sim.Obs{Clock: &clock})

	s1, _ := lsq.Solve(base)
	s2, _ := lsq.Solve(shifted)
	if d := geo.Norm(geo.Sub(s1.Pos, s2.Pos)); d > 1e-5 {
		t.Fatalf("整体平移后位置变化 %.6f m", d)
	}
	if math.Abs((s2.ClockBias-s1.ClockBias)-shift) > 1e-4 {
		t.Fatalf("钟差变化 %v, 期望 %v", s2.ClockBias-s1.ClockBias, shift)
	}
}

func TestValidation(t *testing.T) {
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 6, 15, 1)
	good := c.Observe(sim.Obs{})

	cases := []struct {
		name   string
		mutate func(*lsq.Epoch)
		field  string
	}{
		{"不足4星", func(e *lsq.Epoch) { e.Sats = e.Sats[:3] }, "satellites"},
		{"重复编号", func(e *lsq.Epoch) { e.Sats[1].ID = e.Sats[0].ID }, "satellites[1].id"},
		{"NaN伪距", func(e *lsq.Epoch) { e.Sats[2].PR = math.NaN() }, "satellites[2].pr"},
		{"Inf坐标", func(e *lsq.Epoch) { e.Sats[0].Pos.X = math.Inf(1) }, "satellites[0].pos"},
		{"sigma为0", func(e *lsq.Epoch) { e.Sats[0].Sigma = 0 }, "satellites[0].sigma"},
		{"sigma为负", func(e *lsq.Epoch) { e.Sats[0].Sigma = -1 }, "satellites[0].sigma"},
		{"概略贴地心", func(e *lsq.Epoch) { e.Approx = geo.Vec{} }, "approx"},
	}
	for _, tc := range cases {
		e := *good
		sats := make([]lsq.Satellite, len(good.Sats))
		copy(sats, good.Sats)
		e.Sats = sats
		tc.mutate(&e)
		_, err := lsq.Solve(&e)
		fe, ok := err.(*apierr.FieldError)
		if !ok {
			t.Fatalf("%s: 错误不是 FieldError: %v", tc.name, err)
		}
		if fe.Field != tc.field {
			t.Fatalf("%s: 错误字段=%s, 期望 %s (%v)", tc.name, fe.Field, tc.field, err)
		}
	}
}

func TestCoplanarSingular(t *testing.T) {
	// 4 颗星方向完全共面（同仰角、方位十字对称），G 的行秩不足 4。
	rec := sim.DefaultReceiver()
	defs := []sim.SatelliteDef{
		{ID: 1, Azimuth: 0, Elev: 30},
		{ID: 2, Azimuth: 90, Elev: 30},
		{ID: 3, Azimuth: 180, Elev: 30},
		{ID: 4, Azimuth: 270, Elev: 30},
	}
	c := sim.BuildConstellation(rec, defs)
	ep := c.Observe(sim.Obs{})
	_, err := lsq.Solve(ep)
	if err == nil {
		t.Fatal("共面几何应报奇异")
	}
	if fe, ok := err.(*apierr.FieldError); !ok || fe.Field != "satellites" {
		t.Fatalf("期望指向 satellites 的字段错误, got %v", err)
	}
}

func TestWeightedSSEMonotonicWithBias(t *testing.T) {
	// 偏差逐步加大，SSE 单调变大
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 8, 20, 11)
	prev := -1.0
	for _, b := range []float64{0, 20, 50, 100, 200, 400} {
		ep := c.Observe(sim.Obs{Rng: rand.New(rand.NewSource(1)), Bias: map[int]float64{3: b}})
		sol, err := lsq.Solve(ep)
		if err != nil {
			t.Fatal(err)
		}
		sse, dof := lsq.WeightedSSE(sol.Resid, sol.Sigma)
		if dof != 4 {
			t.Fatalf("dof=%d", dof)
		}
		if sse <= prev {
			t.Fatalf("b=%v sse=%v 未单调递增 (prev=%v)", b, sse, prev)
		}
		prev = sse
	}
}

func TestHDOPWorseWithClusteredGeometry(t *testing.T) {
	// 方位均布 HDOP 小；方位挤在一个扇区 HDOP 大（水平几何差）。
	rec := sim.DefaultReceiver()
	good := sim.BuildConstellation(rec, []sim.SatelliteDef{
		{ID: 1, Azimuth: 0, Elev: 25}, {ID: 2, Azimuth: 45, Elev: 70},
		{ID: 3, Azimuth: 90, Elev: 30}, {ID: 4, Azimuth: 135, Elev: 65},
		{ID: 5, Azimuth: 180, Elev: 35}, {ID: 6, Azimuth: 225, Elev: 72},
		{ID: 7, Azimuth: 270, Elev: 28}, {ID: 8, Azimuth: 315, Elev: 60},
	})
	bad := sim.BuildConstellation(rec, []sim.SatelliteDef{
		{ID: 1, Azimuth: 0, Elev: 25}, {ID: 2, Azimuth: 8, Elev: 70},
		{ID: 3, Azimuth: 16, Elev: 30}, {ID: 4, Azimuth: 24, Elev: 65},
		{ID: 5, Azimuth: 32, Elev: 35}, {ID: 6, Azimuth: 40, Elev: 72},
		{ID: 7, Azimuth: 48, Elev: 28}, {ID: 8, Azimuth: 56, Elev: 60},
	})
	s1, err := lsq.Solve(good.Observe(sim.Obs{}))
	if err != nil {
		t.Fatal(err)
	}
	s2, err := lsq.Solve(bad.Observe(sim.Obs{}))
	if err != nil {
		t.Fatal(err)
	}
	if s2.HDOP <= s1.HDOP {
		t.Fatalf("挤扇区 HDOP 应更大: %v vs %v", s2.HDOP, s1.HDOP)
	}
}

func TestSSEMeanMatchesDOF(t *testing.T) {
	// 无故障时 SSE 服从卡方：多历元均值接近 dof（验收：相对偏差 5% 以内）
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)
	rng := rand.New(rand.NewSource(123))
	var sum float64
	const N = 2000
	for i := 0; i < N; i++ {
		sol, err := lsq.Solve(c.Observe(sim.Obs{Rng: rng}))
		if err != nil {
			t.Fatal(err)
		}
		sse, _ := lsq.WeightedSSE(sol.Resid, sol.Sigma)
		sum += sse
	}
	mean := sum / N
	dof := 5.0
	if math.Abs(mean-dof)/dof > 0.05 {
		t.Fatalf("SSE 均值 %v 与 dof %v 相对偏差超 5%%", mean, dof)
	}
}
