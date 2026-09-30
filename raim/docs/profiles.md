# 运行档（Profile）

运行档是具名的完好性参数集，会话创建时绑定，每个历元结果都记录所用档与当时阈值。

## 字段

| 字段 | 含义 |
|---|---|
| `name` | 档名（唯一） |
| `description` | 说明 |
| `pfa` | 虚警概率，决定卡方检测阈值 T：P(χ²>T)=Pfa。Pfa 越小阈值越大 |
| `pmd` | 漏检概率，决定 HPL 的非中心卡方门限 |
| `hal` | 水平告警限（米），HPL>HAL 触发告警 |
| `isolation.min_epochs` | 隔离星恢复所需的连续正常历元数 |
| `alert.mode` | `snapshot` / `persistence` / `combined` |
| `alert.confirm_epochs` | 连续超限多少历元才拉起告警 |
| `alert.clear_epochs` | 连续正常多少历元才撤警 |
| `alert.gross_factor` | combined 模式下立即告警的恶劣倍数（>1） |
| `sources` | 参数出处（内置档自带） |

## 内置三档

| 档 | HAL | Pfa | Pmd | 告警 |
|---|---|---|---|---|
| `enroute`（航路） | 2.0 NM = 3704 m | 1e-5 | 1e-3 | combined，窗口 3/3，gross 2× |
| `terminal`（终端区） | 1.0 NM = 1852 m | 1e-5 | 1e-3 | combined，窗口 3/3，gross 2× |
| `npa`（非精密进近） | 0.3 NM = 556 m | 1e-5 | 1e-3 | combined，窗口 3/3，gross 2× |

三档隔离恢复均为连续 5 个历元。

### 数值出处

- HAL（2.0 / 1.0 / 0.3 NM）与各飞行阶段 RAIM 要求：**ICAO Annex 10, Volume I
  （Radio Navigation Aids）**；NPA 另见 ICAO **PBN Manual Doc 9613**。
- RAIM/FDE 算法框架与 Pfa/Pmd 用法：**RTCA DO-208 (1991), Minimum Operational
  Performance Standards for Airborne Supplemental Navigation Equipment Using GPS**。
- 斜率法 HPL、非中心卡方漏检门限：**P. Groves (2013), Principles of GNSS, Inertial,
  and Multisensor Integrated Navigation Systems, 2nd ed., 第 17 章（RAIM/FDE）**；
  另见 **E. Kaplan & C. Hegarty (2017), Understanding GPS/GNSS, 3rd ed., 第 11 章**。

> 各标准给出的是 HAL 与 Pfa/Pmd 量级；`isolation.min_epochs` 与告警持续性窗口是本实现
> 为跨历元稳定性给出的**工程默认**（非标准强制值），调用方可按机型与数据回放结果调整。

## 新建运行档

```bash
curl -XPOST localhost:8080/api/profiles -H 'Content-Type: application/json' -d '{
  "name": "custom_npa",
  "description": "更保守的 NPA 档",
  "pfa": 1e-6, "pmd": 1e-4, "hal": 556,
  "isolation": {"min_epochs": 6},
  "alert": {"mode": "combined", "confirm_epochs": 2, "clear_epochs": 3, "gross_factor": 2}
}'
```

字段非法会返回 400 并指明字段，例如 `{"field":"pfa","error":"虚警概率必须在 (0,1) 内"}`。
