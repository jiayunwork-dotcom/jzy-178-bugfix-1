# 机载 GNSS 接收机完好性监测（RAIM/FDE）回放服务

本服务在地面用录好的飞行数据验证机载 GNSS 完好性监测软件。单历元这层沿用既有逻辑
（加权单点最小二乘定位 → 卡方检测 → 逐星试排 → 保护级），本版新增**跨历元状态管理**：

- 被排除的星进入**隔离**，必须连续满足若干历元“本星残差正常且加回后整体检验通过”才恢复，
  隔离期间不参与解算、但仍逐历元算检验量，杜绝“刚剔掉、下历元残差回落又被拉回”的反复进出；
- 告警不再“单历元超限即拉起/恢复即撤”，引入**持续性窗口**与**恶劣超限立即告警**的组合判定，
  抑制残差尖峰造成的闪烁告警。

后端 Go 1.22 + Echo，运行档与会话状态以文件形式持久化，支持重启续跑。

## 目录结构（各包单一职责）

| 包 | 职责 |
|---|---|
| `pkg/geo` | WGS84 ECEF↔经纬度高、ENU 旋转矩阵 |
| `pkg/lsq` | 加权最小二乘迭代定位（1mm/10 轮停止）、残差投影 S、DOP、RAIM 斜率、字段校验 |
| `pkg/chisq` | 自研卡方/非中心卡方 CDF 与分位数（不完全伽马函数，**不引统计库**） |
| `pkg/detect` | 残差平方和卡方检测、逐星排除（唯一可解释才排除）、不足 5/6 星的处理 |
| `pkg/protect` | 斜率法水平保护级 HPL（非中心卡方漏检门限） |
| `pkg/profile` | 具名运行档（航路/终端区/NPA + 调用方自建） |
| `pkg/statem` | 跨历元隔离/恢复计数、告警持续性状态机（纯逻辑） |
| `pkg/session` | 会话编排、时间戳去重/倒退、统计、文件持久化 |
| `pkg/server` | Echo HTTP 路由 |
| `internal/sim` | 固定种子的确定性仿真（测试与回放演示共用） |
| `cmd/server` | 服务入口 |
| `cmd/replaydemo` | 三种告警判定回放对比，输出 `docs/replay_results.json` |

## 快速开始

```bash
# 构建并运行（数据目录即运行档与会话落盘处，应挂载持久卷）
go build -o raim-server ./cmd/server
./raim-server --addr :8080 --data ./data

# Docker 两阶段构建
docker build -t raim:latest .
docker run -p 8080:8080 -v $(pwd)/data:/data raim:latest

# 跑测试
CGO_ENABLED=0 go test ./...

# 三种告警判定回放对比（固定种子，结果与 docs/replay_results.json 一致）
go run ./cmd/replaydemo --seed 20260930
```

详细接口见 [docs/api.md](docs/api.md)，算法与跨历元选型见
[docs/design.md](docs/design.md)，运行档参数与出处见 [docs/profiles.md](docs/profiles.md)。
