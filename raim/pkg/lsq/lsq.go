// Package lsq 实现加权单点最小二乘定位及定位后的几何量：
// 标准化残差（残差投影 S）、DOP、RAIM 斜率。
//
// 观测模型（线性化于估计位置）：
//
//	y = G·dx + ε，G 每行 [-ux -uy -uz 1]
//	W = diag(1/σ_i²)
//	解：dx = (GᵀWG)^{-1} GᵀW y
//	加权残差：r = y - G·dx
//	标准化残差投影：S = I - G (GᵀWG)^{-1} GᵀW（S·y 即加权残差）
package lsq

import (
	"math"

	"raim/pkg/apierr"
	"raim/pkg/geo"
)

// maxIter 是定位迭代的硬上限（需求：最多 10 轮）。
const maxIter = 10

// Satellite 是一颗参与解算的卫星：ECEF 坐标、伪距（米）与伪距误差标准差（米，用于定权）。
type Satellite struct {
	ID    int     `json:"id"`
	Pos   geo.Vec `json:"pos"`
	PR    float64 `json:"pr"`
	Sigma float64 `json:"sigma"`
}

// Epoch 是一个历元的定位输入。Approx 为概略位置初值（可为零值，此时取 0,0,0 附近不可用）。
type Epoch struct {
	Approx geo.Vec
	Sats   []Satellite
}

// Solution 是定位结果。
type Solution struct {
	Pos       geo.Vec   // ECEF 位置
	LLA       geo.LLA   // 经纬度高
	ClockBias float64   // 接收机钟差（米）
	Iter      int       // 实际迭代轮数
	Converged bool      // 是否在 10 轮内满足 1mm 收敛
	Resid     []float64 // 每颗星残差（观测伪距 - 预测伪距，含钟差），米
	// G 为最终几何矩阵，W 为权重对角，S 为残差投影；供检测/保护级使用。
	G [][]float64
	W []float64
	S [][]float64
	// DOP（加权尺度）
	HDOP, VDOP, TDOP, GDOP float64
	// Sigma 为各星误差标准差，顺序与输入一致。
	Sigma []float64
}

func badFloat(x float64) bool { return math.IsNaN(x) || math.IsInf(x, 0) }

// Validate 按需求逐字段校验输入，错误指到具体字段。
func (e *Epoch) Validate() error {
	if len(e.Sats) < 4 {
		return apierr.New("satellites", "可见星不足 4 颗，无法定位")
	}
	seen := map[int]bool{}
	for i, s := range e.Sats {
		field := func(name string) string {
			return "satellites[" + itoa(i) + "]." + name
		}
		if s.ID <= 0 {
			return apierr.New(field("id"), "卫星编号必须为正整数")
		}
		if seen[s.ID] {
			return apierr.New(field("id"), "同一卫星编号出现两次")
		}
		seen[s.ID] = true
		if badFloat(s.Pos.X) || badFloat(s.Pos.Y) || badFloat(s.Pos.Z) {
			return apierr.New(field("pos"), "卫星坐标含 NaN 或无穷")
		}
		if badFloat(s.PR) {
			return apierr.New(field("pr"), "伪距含 NaN 或无穷")
		}
		if badFloat(s.Sigma) || s.Sigma <= 0 {
			return apierr.New(field("sigma"), "伪距误差标准差必须为有限正数")
		}
	}
	if badFloat(e.Approx.X) || badFloat(e.Approx.Y) || badFloat(e.Approx.Z) {
		return apierr.New("approx", "概略位置含 NaN 或无穷")
	}
	// “几乎贴着地心”：ECEF 模长小于地球极半径的 1%（约 64 km）。
	if r := geo.Norm(e.Approx); r < 0.01*geo.B {
		return apierr.New("approx", "概略位置几乎贴着地心，请给出合理的地面初值")
	}
	return nil
}

// Solve 执行加权最小二乘迭代定位。
func Solve(e *Epoch) (*Solution, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	n := len(e.Sats)

	pos := e.Approx
	clock := 0.0
	iter := 0
	converged := false

	var G [][]float64
	var y, wdiag, resid []float64

	for iter = 1; iter <= maxIter; iter++ {
		G = make([][]float64, n)
		y = make([]float64, n)
		wdiag = make([]float64, n)
		for i, s := range e.Sats {
			d := geo.Sub(s.Pos, pos)
			r := geo.Norm(d)
			ux, uy, uz := d.X/r, d.Y/r, d.Z/r
			G[i] = []float64{-ux, -uy, -uz, 1}
			y[i] = s.PR - (r + clock)
			wdiag[i] = 1 / (s.Sigma * s.Sigma)
		}
		dx, err := solveNormal(G, y, wdiag)
		if err != nil {
			return nil, err
		}
		pos = geo.Add(pos, geo.Vec{X: dx[0], Y: dx[1], Z: dx[2]})
		clock += dx[3]
		if math.Sqrt(dx[0]*dx[0]+dx[1]*dx[1]+dx[2]*dx[2]) < 0.001 {
			converged = true
			break
		}
	}
	if iter > maxIter {
		iter = maxIter // 跑满 10 轮仍未收敛：实际轮数记 10，Converged=false
	}

	// 最终残差与投影矩阵
	resid = make([]float64, n)
	for i, s := range e.Sats {
		r := geo.Norm(geo.Sub(s.Pos, pos))
		resid[i] = s.PR - (r + clock)
	}
	S := projection(G, wdiag)

	sol := &Solution{
		Pos:       pos,
		LLA:       geo.ECEFToLLA(pos),
		ClockBias: clock,
		Iter:      iter,
		Converged: converged,
		Resid:     resid,
		G:         G,
		W:         wdiag,
		S:         S,
		Sigma:     make([]float64, n),
	}
	for i, s := range e.Sats {
		sol.Sigma[i] = s.Sigma
	}
	fillDOP(sol)
	return sol, nil
}

// solveNormal 解 (GᵀWG) dx = GᵀWy，并对奇异几何报错。
func solveNormal(G [][]float64, y, w []float64) ([]float64, error) {
	n := len(G)
	m := 4
	N := make([][]float64, m)
	u := make([]float64, m)
	for i := range N {
		N[i] = make([]float64, m)
	}
	for k := 0; k < n; k++ {
		for i := 0; i < m; i++ {
			u[i] += w[k] * G[k][i] * y[k]
			for j := 0; j < m; j++ {
				N[i][j] += w[k] * G[k][i] * G[k][j]
			}
		}
	}
	// 先对列做归一化，再按相对主元判奇异（卫星共面等情形）。
	scale := make([]float64, m)
	for i := 0; i < m; i++ {
		scale[i] = math.Sqrt(math.Abs(N[i][i]))
		if scale[i] == 0 {
			scale[i] = 1
		}
	}
	for i := 0; i < m; i++ {
		for j := 0; j < m; j++ {
			N[i][j] /= scale[i] * scale[j]
		}
		u[i] /= scale[i]
	}
	for col := 0; col < m; col++ {
		// 部分选主元
		piv := col
		for r := col + 1; r < m; r++ {
			if math.Abs(N[r][col]) > math.Abs(N[piv][col]) {
				piv = r
			}
		}
		if math.Abs(N[piv][col]) < 1e-9 {
			return nil, apierr.New("satellites", "几何矩阵奇异（卫星可能共面或几何退化），无法定位")
		}
		if piv != col {
			N[piv], N[col] = N[col], N[piv]
			u[piv], u[col] = u[col], u[piv]
		}
		for r := 0; r < m; r++ {
			if r == col {
				continue
			}
			f := N[r][col] / N[col][col]
			for j := col; j < m; j++ {
				N[r][j] -= f * N[col][j]
			}
			u[r] -= f * u[col]
		}
	}
	x := make([]float64, m)
	for i := 0; i < m; i++ {
		x[i] = u[i] / N[i][i] / scale[i]
	}
	return x, nil
}

// CovPos 返回 (GᵀWG)^{-1} 的左上角 3x3（位置协方差，单位按 sigma 尺度）。
func (s *Solution) CovPos() [3][3]float64 {
	m := 4
	nRows := len(s.G)
	N := make([][]float64, m)
	for i := range N {
		N[i] = make([]float64, m)
	}
	for k := 0; k < nRows; k++ {
		for i := 0; i < m; i++ {
			for j := 0; j < m; j++ {
				N[i][j] += s.W[k] * s.G[k][i] * s.G[k][j]
			}
		}
	}
	inv := invert4(N)
	var cov [3][3]float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			cov[i][j] = inv[i][j]
		}
	}
	return cov
}

// invert4 高斯-若尔当求 4x4 逆矩阵（调用处已保证非奇异）。
func invert4(N [][]float64) [4][4]float64 {
	m := 4
	a := make([][]float64, m)
	for i := range a {
		a[i] = make([]float64, 2*m)
		copy(a[i], N[i])
		a[i][m+i] = 1
	}
	for col := 0; col < m; col++ {
		piv := col
		for r := col + 1; r < m; r++ {
			if math.Abs(a[r][col]) > math.Abs(a[piv][col]) {
				piv = r
			}
		}
		a[piv], a[col] = a[col], a[piv]
		d := a[col][col]
		for j := 0; j < 2*m; j++ {
			a[col][j] /= d
		}
		for r := 0; r < m; r++ {
			if r == col {
				continue
			}
			f := a[r][col]
			for j := 0; j < 2*m; j++ {
				a[r][j] -= f * a[col][j]
			}
		}
	}
	var out [4][4]float64
	for i := 0; i < m; i++ {
		for j := 0; j < m; j++ {
			out[i][j] = a[i][m+j]
		}
	}
	return out
}

// projection 计算 S = I - G (GᵀWG)^{-1} GᵀW。
func projection(G [][]float64, w []float64) [][]float64 {
	n := len(G)
	m := 4
	N := make([][]float64, m)
	for i := range N {
		N[i] = make([]float64, m)
	}
	for k := 0; k < n; k++ {
		for i := 0; i < m; i++ {
			for j := 0; j < m; j++ {
				N[i][j] += w[k] * G[k][i] * G[k][j]
			}
		}
	}
	inv := invert4(N)
	S := make([][]float64, n)
	for i := 0; i < n; i++ {
		S[i] = make([]float64, n)
		for j := 0; j < n; j++ {
			v := 0.0
			for a := 0; a < m; a++ {
				for b := 0; b < m; b++ {
					v += G[i][a] * inv[a][b] * G[j][b] * w[j]
				}
			}
			if i == j {
				v = 1 - v
			} else {
				v = -v
			}
			S[i][j] = v
		}
	}
	return S
}

// fillDOP 用 ENU 旋转把位置协方差换算到水平/垂直 DOP。
func fillDOP(sol *Solution) {
	cov := sol.CovPos()
	rot := geo.ENURotation(sol.LLA.Lon, sol.LLA.Lat)
	// D_ENU = R D_ECEF Rᵀ
	var c [3][3]float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			v := 0.0
			for a := 0; a < 3; a++ {
				for b := 0; b < 3; b++ {
					v += rot[i][a] * cov[a][b] * rot[j][b]
				}
			}
			c[i][j] = v
		}
	}
	sol.HDOP = math.Sqrt(math.Max(0, c[0][0]+c[1][1]))
	sol.VDOP = math.Sqrt(math.Max(0, c[2][2]))
	// 钟差项 = (GᵀWG)^{-1}_{33}
	nRows := len(sol.G)
	N := make([][]float64, 4)
	for i := range N {
		N[i] = make([]float64, 4)
	}
	for k := 0; k < nRows; k++ {
		for i := 0; i < 4; i++ {
			for j := 0; j < 4; j++ {
				N[i][j] += sol.W[k] * sol.G[k][i] * sol.G[k][j]
			}
		}
	}
	inv := invert4(N)
	sol.TDOP = math.Sqrt(math.Max(0, inv[3][3]))
	sol.GDOP = math.Sqrt(math.Max(0, c[0][0]+c[1][1]+c[2][2]+inv[3][3]))
}

// WeightedSSE 返回加权残差平方和 Σ (r_i/σ_i)²，及自由度 n-4。
func WeightedSSE(resid, sigma []float64) (float64, int) {
	sse := 0.0
	for i := range resid {
		z := resid[i] / sigma[i]
		sse += z * z
	}
	return sse, len(resid) - 4
}

// Slopes 返回每颗星的 RAIM 斜率（按输入顺序，单位 m/m）。
//
// 采用 Brown (1992) 的经典定义：第 i 颗星存在 1 m 伪距偏差 b 时，
// 定位解的水平位移与该星检验统计量增量之比。
//
//	令 K = (GᵀWG)^{-1}，A = K GᵀW（最小二乘增益矩阵）。
//	b 作用下位置水平位移 = ‖(A 的前两行)[:,i]‖·|b|；
//	该星加权残差的改变量 = S_ii·b，检验统计量 √SSE 的增量
//	（小偏差近似）正比于 S_ii/(σ_i·√S_ii) = √S_ii/σ_i。
//
// 故 slope_i = ‖A_xy[:,i]‖·σ_i/√S_ii，取最大者计算 HPL。
func (s *Solution) Slopes() []float64 {
	n := len(s.G)
	inv := invert4(buildN(s))
	rot := geo.ENURotation(s.LLA.Lon, s.LLA.Lat)

	slopes := make([]float64, n)
	for i := 0; i < n; i++ {
		// A 的前 3 行第 i 列（ECEF 位置对第 i 个观测偏差的增益）
		var dEcef [3]float64
		for a := 0; a < 3; a++ {
			v := 0.0
			for b := 0; b < 4; b++ {
				v += inv[a][b] * s.G[i][b]
			}
			dEcef[a] = v * s.W[i]
		}
		east := rot[0][0]*dEcef[0] + rot[0][1]*dEcef[1] + rot[0][2]*dEcef[2]
		north := rot[1][0]*dEcef[0] + rot[1][1]*dEcef[1] + rot[1][2]*dEcef[2]
		hShift := math.Sqrt(east*east + north*north)
		sii := math.Sqrt(math.Max(1e-30, s.S[i][i]))
		slopes[i] = hShift * s.Sigma[i] / sii
	}
	return slopes
}

func buildN(s *Solution) [][]float64 {
	N := make([][]float64, 4)
	for i := range N {
		N[i] = make([]float64, 4)
	}
	for k := 0; k < len(s.G); k++ {
		for i := 0; i < 4; i++ {
			for j := 0; j < 4; j++ {
				N[i][j] += s.W[k] * s.G[k][i] * s.G[k][j]
			}
		}
	}
	return N
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
