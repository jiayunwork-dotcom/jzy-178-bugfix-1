package chisq

import (
	"math"
	"testing"
)

func TestCDFKnown(t *testing.T) {
	// 与标准卡方表对照：
	cases := []struct {
		k    int
		x    float64
		want float64
	}{
		{1, 0.4549, 0.5},
		{1, 3.8415, 0.95},
		{2, 5.9915, 0.95},
		{5, 11.0705, 0.95},
		{10, 23.2093, 0.99},
		{20, 45.3147, 0.999},
	}
	for _, c := range cases {
		got := CDF(c.k, c.x)
		if math.Abs(got-c.want) > 1e-4 {
			t.Errorf("CDF(%d,%v)=%v, want %v", c.k, c.x, got, c.want)
		}
	}
}

func TestQuantileRoundTrip(t *testing.T) {
	for _, k := range []int{1, 2, 4, 7, 12} {
		for _, p := range []float64{0.5, 0.9, 0.95, 0.99, 0.999, 0.99999} {
			q := Quantile(k, p)
			back := CDF(k, q)
			if math.Abs(back-p) > 1e-6 {
				t.Errorf("k=%d p=%v q=%v roundtrip=%v", k, p, q, back)
			}
		}
	}
}

func TestThresholdMonotonic(t *testing.T) {
	// 虚警概率越小，阈值越大
	t1 := Threshold(8, 1e-3)
	t2 := Threshold(8, 1e-5)
	t3 := Threshold(8, 1e-7)
	if !(t1 < t2 && t2 < t3) {
		t.Fatalf("阈值未随虚警概率减小而增大: %v %v %v", t1, t2, t3)
	}
}

func TestNoncentralCDFShiftsRight(t *testing.T) {
	// 非中心参数越大，同点 CDF 越小；λ=0 与中心卡方一致
	if math.Abs(NoncentralCDF(6, 0, 10)-CDF(6, 10)) > 1e-12 {
		t.Fatal("λ=0 应退化为中心卡方")
	}
	prev := 1.0
	for _, lam := range []float64{2, 5, 10, 20, 50} {
		v := NoncentralCDF(6, lam, 12)
		if v >= prev {
			t.Fatalf("非中心 CDF 应随 λ 减小: λ=%v v=%v prev=%v", lam, v, prev)
		}
		prev = v
	}
}

func TestNoncentralQuantileRoundTrip(t *testing.T) {
	for _, c := range []struct {
		k  int
		la float64
		p  float64
	}{
		{5, 10, 0.999}, {8, 20, 0.999}, {12, 0, 0.99999}, {6, 5, 0.5},
	} {
		q := NoncentralQuantile(c.k, c.la, c.p)
		back := NoncentralCDF(c.k, c.la, q)
		if math.Abs(back-c.p) > 1e-5 {
			t.Errorf("k=%d λ=%v p=%v q=%v back=%v", c.k, c.la, c.p, q, back)
		}
	}
}

func TestPoissonWeightsNormalized(t *testing.T) {
	for _, mu := range []float64{0.5, 5, 50} {
		_, ws := poissonWeights(mu)
		sum := 0.0
		for _, w := range ws {
			sum += w
		}
		if math.Abs(sum-1) > 1e-6 {
			t.Errorf("mu=%v Poisson 权重和=%v", mu, sum)
		}
	}
}
