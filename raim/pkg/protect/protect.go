// Package protect 计算水平保护级 HPL（Brown/Sturza 的斜率法，见
// RTCA DO-208、Groves《GNSS、惯性与多传感器组合导航原理》第 17 章）。
//
//	HPL = slopeMax · √λ(Pmd, dof, T)
//
// slopeMax 是各星 RAIM 斜率的最大值：单星存在 1 m 伪距偏差时水平位置位移与
// 该星检验统计量增量之比（σ 已并入斜率，故 slopeMax 单位为米）。
// √λ 是非中心卡方分布在漏检概率 Pmd 处的非中心参数平方根：给定检测门限 T，
// 求 λ 使 P(χ'²(dof,λ²) ≤ T) = Pmd。排除成功后 dof 取 n−5、T 取对应子集门限。
package protect

import (
	"math"

	"raim/pkg/chisq"
	"raim/pkg/detect"
	"raim/pkg/lsq"
)

// HPL 用给定定位解的几何、检测门限与漏检概率计算水平保护级（米）。
// dof 为检测自由度（通常为参与星数-4），threshold 为与检测共用的卡方门限。
func HPL(sol *lsq.Solution, dof int, threshold, pMD float64) float64 {
	if sol == nil || dof <= 0 {
		return 0
	}
	slopes := sol.Slopes()
	slopeMax := 0.0
	for _, s := range slopes {
		if s > slopeMax {
			slopeMax = s
		}
	}
	sqrtLambda := lambdaSqrt(pMD, dof, threshold)
	return slopeMax * sqrtLambda
}

// lambdaSqrt 求满足 P(χ'²(dof, λ²) ≤ T)=Pmd 的 √λ。
// T 是检测门限，由调用方传入（与检测用的同一阈值）。
//
// 对给定 dof、阈值 T 和漏检概率 Pmd，存在唯一的 λ 使非中心 CDF=Pmd，
// 对非中心参数做对分。
func lambdaSqrt(pMD float64, dof int, threshold float64) float64 {
	if pMD <= 0 || pMD >= 1 || dof <= 0 {
		return 0
	}
	// 若中心卡方在 T 处的 CDF 已 ≤ Pmd，则即使零偏差漏检率也达标，λ=0。
	if chisq.CDF(dof, threshold) <= pMD {
		return 0
	}
	f := func(lambda float64) float64 {
		return chisq.NoncentralCDF(dof, lambda*lambda, threshold)
	}
	lo, hi := 0.0, 1.0
	for f(hi) > pMD {
		lo = hi
		hi *= 2
		if math.IsInf(hi, 1) || hi > 1e6 {
			break
		}
	}
	for i := 0; i < 60; i++ {
		mid := (lo + hi) / 2
		if f(mid) > pMD {
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

// AssessmentHPL 对检测结论使用的解算 HPL。排除后用剩余星的几何与自由度。
func AssessmentHPL(a *detect.Assessment, pMD float64) float64 {
	return HPL(a.Sol, a.SolDOF, a.SolThreshold, pMD)
}
