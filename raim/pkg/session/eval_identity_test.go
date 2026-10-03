package session

import (
	"math"
	"testing"

	"raim/internal/sim"
	"raim/pkg/chisq"
	"raim/pkg/geo"
	"raim/pkg/lsq"
)

// linearSSEAt 在给定线性化点 (x0,y0,z0,clock0) 上对 sats 组装并求解
// 一次线性加权最小二乘，返回更新后的加权 SSE。
func linearSSEAt(x0, y0, z0, clock0 float64, sats []lsq.Satellite) float64 {
	p0 := geo.Vec{X: x0, Y: y0, Z: z0}
	n := len(sats)
	G := make([][]float64, n)
	y := make([]float64, n)
	w := make([]float64, n)
	for i, s := range sats {
		d := geo.Sub(s.Pos, p0)
		r := geo.Norm(d)
		G[i] = []float64{-d.X / r, -d.Y / r, -d.Z / r, 1}
		y[i] = s.PR - (r + clock0)
		w[i] = 1 / (s.Sigma * s.Sigma)
	}
	// 法方程
	N := make([][]float64, 4)
	u := make([]float64, 4)
	for i := range N {
		N[i] = make([]float64, 4)
	}
	for k := 0; k < n; k++ {
		for i := 0; i < 4; i++ {
			u[i] += w[k] * G[k][i] * y[k]
			for j := 0; j < 4; j++ {
				N[i][j] += w[k] * G[k][i] * G[k][j]
			}
		}
	}
	dx := gauss4(N, u)
	sse := 0.0
	for k := 0; k < n; k++ {
		pred := 0.0
		for j := 0; j < 4; j++ {
			pred += G[k][j] * dx[j]
		}
		r := (y[k] - pred) / sats[k].Sigma
		sse += r * r
	}
	return sse
}

func gauss4(N [][]float64, u []float64) []float64 {
	a := make([][]float64, 4)
	for i := range a {
		a[i] = append(append([]float64{}, N[i]...), u[i])
	}
	for col := 0; col < 4; col++ {
		piv := col
		for r := col + 1; r < 4; r++ {
			if math.Abs(a[r][col]) > math.Abs(a[piv][col]) {
				piv = r
			}
		}
		a[piv], a[col] = a[col], a[piv]
		for r := 0; r < 4; r++ {
			if r == col {
				continue
			}
			f := a[r][col] / a[col][col]
			for j := col; j <= 4; j++ {
				a[r][j] -= f * a[col][j]
			}
		}
	}
	x := make([]float64, 4)
	for i := 0; i < 4; i++ {
		x[i] = a[i][4] / a[i][i]
	}
	return x
}

// TestAddSSEIdentity：恢复检验用的恒等式 SSE_add = SSE_base + z²
// 必须与“真正重解一次基线+该星的加回解”得到的 SSE 一致。
// 这是隔离恢复检验统计口径正确的数值保证。
func TestAddSSEIdentity(t *testing.T) {
	c := sim.EvenSky(sim.DefaultReceiver(), 10, 10, 42)
	rng := newSeededRand(123)

	for iter := 0; iter < 50; iter++ {
		ep := c.Observe(sim.Obs{Rng: rng, Sigma: 1.0})
		// 随机挑一颗作“待评估隔离星”，其余为基线
		k := iter % len(ep.Sats)
		target := ep.Sats[k]
		baseSats := append(append([]lsq.Satellite{}, ep.Sats[:k]...), ep.Sats[k+1:]...)
		base, err := lsq.Solve(&lsq.Epoch{Approx: ep.Approx, Sats: baseSats})
		if err != nil {
			t.Fatal(err)
		}
		// evalOne 的算法
		e := evalOne(testProf(), EpochInputFromLSQ(ep), baseSats, ep.Approx, target.ID)
		if e.Note != "" {
			t.Fatalf("iter=%d 评估失败: %s", iter, e.Note)
		}
		// 精确基准：固定在基线解位置（同一线性化点）做一次线性 WLS。
		// 恒等式在该点精确成立；重新迭代收敛会引入 <1mm 的二阶位置差，
		// 不属于这里要验证的代数恒等式。
		directSSE := linearSSEAt(base.Pos.X, base.Pos.Y, base.Pos.Z,
			base.ClockBias, append(append([]lsq.Satellite{}, baseSats...), target))
		if math.Abs(directSSE-e.AddSSE) > 1e-8*math.Max(1, directSSE) {
			t.Fatalf("iter=%d id=%d 恒等式不成立: identity=%.10f linear=%.10f",
				iter, target.ID, e.AddSSE, directSSE)
		}
		// 无故障噪声下：门限与加回解自由度 n−4 自洽
		thr := chisq.Threshold(len(baseSats)+1-4, testProf().Pfa)
		if math.Abs(e.AddThreshold-thr) > 1e-12 {
			t.Fatalf("门限不一致: %.10f vs %.10f", e.AddThreshold, thr)
		}
	}
}

// TestAddSSEIdentityDetectsBias：给目标星加 80m 偏差时，
// 留一检验量必须显著超门限（证明坏星偏差不会被自己的解吸收）。
func TestAddSSEIdentityDetectsBias(t *testing.T) {
	c := sim.EvenSky(sim.DefaultReceiver(), 10, 10, 42)
	rng := newSeededRand(777)
	prof := testProf()
	failSelf, failOverall := 0, 0
	for iter := 0; iter < 100; iter++ {
		ep := c.Observe(sim.Obs{
			Rng: rng, Sigma: 1.0,
			Bias: map[int]float64{3: 80},
		})
		var target lsq.Satellite
		baseSats := make([]lsq.Satellite, 0, len(ep.Sats)-1)
		for _, s := range ep.Sats {
			if s.ID == 3 {
				target = s
			} else {
				baseSats = append(baseSats, s)
			}
		}
		e := evalOne(prof, EpochInputFromLSQ(ep), baseSats, ep.Approx, target.ID)
		if math.Abs(e.Z) > e.SelfThr {
			failSelf++
		}
		if e.AddSSE > e.AddThreshold {
			failOverall++
		}
	}
	// 80m 偏差在 σ=1m 下应 100% 检出（留一预测不吸收偏差）
	if failSelf != 100 || failOverall != 100 {
		t.Fatalf("80m 偏差检出率: 单星=%d/100 整体=%d/100", failSelf, failOverall)
	}
}
