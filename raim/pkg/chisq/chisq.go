// Package chisq 自实现卡方与非中心卡方分布（不依赖任何统计库）。
//
// 底层采用正则化下不完全伽马函数 P(a,x)：
//
//	chi2 CDF:        F(x;k) = P(k/2, x/2)
//	非中心 chi2 CDF: F(x;k,λ) = Σ_j w_j · P(k/2+j, x/2)，
//	                 其中 w_j 是 Poisson(λ/2) 概率质量。
//
// 分位数（检测阈值与漏检测门限）用对分反解 CDF。
package chisq

import (
	"math"
)

const (
	eps   = 1e-14
	itmax = 300
	tiny  = 1e-300
)

// logGamma 使用 Lanczos 近似（Numerical Recipes, gslmln）。
func logGamma(x float64) float64 {
	cof := []float64{
		76.18009172947146, -86.50532032941677, 24.01409824083091,
		-1.231739572450155, 0.1208650973866179e-2, -0.5395239384953e-5,
	}
	ser := 1.000000000190015
	y := x
	tmp := x + 5.5
	tmp -= (x + 0.5) * math.Log(tmp)
	for _, c := range cof {
		y++
		ser += c / y
	}
	return -tmp + math.Log(2.5066282746310005*ser/x)
}

// regLowerGamma 正则化下不完全伽马函数 P(a,x)。
// x<a+1 用级数，否则用连分式求 Q 后取补（Numerical Recipes gammp/gammq）。
func regLowerGamma(a, x float64) float64 {
	if x < 0 || a <= 0 {
		return math.NaN()
	}
	if x == 0 {
		return 0
	}
	if x < a+1 {
		ap := a
		delta := 1 / a
		sum := delta
		for n := 0; n < itmax; n++ {
			ap++
			delta *= x / ap
			sum += delta
			if math.Abs(delta) < math.Abs(sum)*eps {
				break
			}
		}
		return sum * math.Exp(-x+a*math.Log(x)-logGamma(a))
	}
	// 连分式求 Q(a,x)
	b := x + 1 - a
	c := 1 / tiny
	d := 1 / b
	h := d
	for i := 1; i <= itmax; i++ {
		an := -float64(i) * (float64(i) - a)
		b += 2
		d = an*d + b
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = b + an/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		step := d * c
		h *= step
		if math.Abs(step-1) < eps {
			break
		}
	}
	q := math.Exp(-x+a*math.Log(x)-logGamma(a)) * h
	p := 1 - q
	if p < 0 {
		return 0
	}
	return p
}

// CDF 返回自由度 k 的卡方分布在 x 处的累积概率。
func CDF(k int, x float64) float64 {
	if k <= 0 {
		return math.NaN()
	}
	return regLowerGamma(float64(k)/2, x/2)
}

// survival 即上侧概率 P(X>x)，给阈值直接传概率更直观时使用。
func survival(k int, x float64) float64 { return 1 - CDF(k, x) }

// Quantile 反解 F(x;k)=p（0<p<1），绝对误差不超过 ~1e-9。
func Quantile(k int, p float64) float64 {
	if k <= 0 || p <= 0 || p >= 1 {
		return math.NaN()
	}
	lo, hi := 0.0, 1.0
	for CDF(k, hi) < p {
		lo = hi
		hi *= 2
		if math.IsInf(hi, 1) {
			return math.NaN()
		}
	}
	for i := 0; i < 80; i++ {
		mid := (lo + hi) / 2
		if CDF(k, mid) < p {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo < 1e-9*(1+hi) {
			break
		}
	}
	return (lo + hi) / 2
}

// Threshold 卡方检测阈值：满足 P(X>T)=alpha 的 T。
func Threshold(k int, alpha float64) float64 { return Quantile(k, 1-alpha) }

// poissonWeights 返回 Poisson(mu) 在众数两侧的 (j,w) 序列（按 j 升序），
// 权重以众数为锚点向两边递推，避免大 λ 时直接算幂导致下溢。
func poissonWeights(mu float64) ([]int, []float64) {
	if mu <= 0 {
		return []int{0}, []float64{1}
	}
	mode := int(math.Floor(mu))
	wMode := math.Exp(-mu + float64(mode)*math.Log(mu) - logGamma(float64(mode+1)))

	js := []int{mode}
	ws := []float64{wMode}

	// 向上：w_{j+1} = w_j * mu/(j+1)
	w := wMode
	for j := mode; ; j++ {
		w *= mu / float64(j+1)
		if w < 1e-16 {
			break
		}
		js = append(js, j+1)
		ws = append(ws, w)
	}

	// 向下：w_{j-1} = w_j * j/mu，结果头插
	w = wMode
	var loJ []int
	var loW []float64
	for j := mode; j > 0; {
		w *= float64(j) / mu
		j--
		if w < 1e-16 {
			break
		}
		loJ = append([]int{j}, loJ...)
		loW = append([]float64{w}, loW...)
	}
	return append(loJ, js...), append(loW, ws...)
}

// NoncentralCDF 非中心卡方 F(x;k,lambda)，自由度 k、非中心参数 lambda>=0。
func NoncentralCDF(k int, lambda, x float64) float64 {
	if k <= 0 || lambda < 0 || x < 0 {
		return math.NaN()
	}
	if lambda == 0 {
		return CDF(k, x)
	}
	js, ws := poissonWeights(lambda / 2)
	a0 := float64(k) / 2
	sum := 0.0
	for idx, j := range js {
		sum += ws[idx] * regLowerGamma(a0+float64(j), x/2)
	}
	if sum > 1 {
		sum = 1
	}
	return sum
}

// NoncentralQuantile 反解 F(x;k,lambda)=p（0<p<1），绝对误差不超过 ~1e-9。
func NoncentralQuantile(k int, lambda, p float64) float64 {
	if k <= 0 || lambda < 0 || p <= 0 || p >= 1 {
		return math.NaN()
	}
	lo, hi := 0.0, 1.0
	for NoncentralCDF(k, lambda, hi) < p {
		lo = hi
		hi *= 2
		if math.IsInf(hi, 1) {
			return math.NaN()
		}
	}
	for i := 0; i < 80; i++ {
		mid := (lo + hi) / 2
		if NoncentralCDF(k, lambda, hi)-NoncentralCDF(k, lambda, lo) < 1e-12 {
			break
		}
		if NoncentralCDF(k, lambda, mid) < p {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo < 1e-9*(1+hi) {
			break
		}
	}
	return (lo + hi) / 2
}
