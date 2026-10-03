# HTTP API

所有接口返回 JSON。错误形如 `{"error":"...","field":"..."}`，字段类问题为 400，
重复时间戳 409，时间戳倒退 422，资源不存在 404。

## 运行档

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/profiles` | 列出全部运行档（内置+自建） |
| GET | `/api/profiles/:name` | 取单个运行档 |
| POST | `/api/profiles` | 新建运行档（字段见 profiles.md） |

## 会话

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/sessions` | 列出会话 ID |
| POST | `/api/sessions` | 创建会话，body `{"id":"可选","profile_name":"enroute"}`（缺 id 自动生成） |
| GET | `/api/sessions/:id` | 会话状态、统计与逐历元记录 |
| POST | `/api/sessions/:id/epochs` | 提交单个历元 |
| POST | `/api/sessions/:id/epochs/batch` | 批量提交（`epochs` 数组，≤3600，整段原子） |

另有 `GET /healthz`。

## 历元请求体

```json
{
  "timestamp": 100,
  "approx": [4096000.0, 3096000.0, 3830000.0],
  "satellites": [
    {"id": 1, "pos": [x, y, z], "pr": 22123456.7, "sigma": 1.2}
  ]
}
```

- `timestamp`：单调时间戳（任意单位，如 Unix 秒）；相同则 409、倒退则 422，均不推进状态。
- `approx`：可选概略位置；省略时沿用上一历元定位，首历元由卫星反推地表点。
- 至少 4 颗星；`sigma>0`；坐标/伪距不得为 NaN/无穷；编号不得重复。

## 历元响应（节选）

```json
{
  "seq": 9, "timestamp": 100,
  "profile_name": "enroute", "pfa": 1e-5, "pmd": 1e-3, "hal": 3704,
  "dof": 5, "chi_square_threshold": 20.515,
  "mode": "excluded",
  "sse": 64.2, "hpl": 10.7, "excluded_id": 4, "isolated": [4],
  "position": {"ecef":[...],"lon":116.39,"lat":39.90,"alt":50.1},
  "clock_bias": 12.3, "iterations": 4, "converged": true,
  "sat_results": [{"id":1,"resid":0.2,"sigma":1.2,"stdres":0.16}],
  "trials": [{"excluded_id":1,"sse":40.1,"passes":false,"used":8}],
  "isolation_tests": [
    {"id":4,"statistic":0.8,"threshold":4.53,"sat_pass":true,
     "full_sse":6.1,"full_threshold":20.5,"full_pass":true,
     "pass":true,"normal_streak":2,"evaluable":true}
  ],
  "alert": false, "bad_streak": 0, "good_streak": 1, "raim_available": true
}
```

`mode` ∈ `ok` / `unavailable`（<5 星）/ `detected`（检出无法排除或不唯一）/ `excluded`。

`isolation_tests` 每历元为每颗“可见且处于隔离”的星（含本历元刚被唯一排除的星）
输出一行；无隔离星时该字段省略。字段：

| 字段 | 含义 |
|---|---|
| `statistic` | 单星恢复检验统计量：不含该星的干净基础解上的标准化新息 `|innov|/σ_innov`（预测再比，别的星残差无法替它掩护） |
| `threshold` | 单星门限 `√T` |
| `sat_pass` | 本星新息是否过门限 |
| `full_sse` | 加回该星后的整体加权残差平方和（`SSE_base+statistic²`，SSE 增量恒等式，不重解） |
| `full_threshold` | 加回后整体卡方门限（自由度 基础星数−3） |
| `full_pass` | 加回后整体检验是否通过 |
| `pass` | 综合判定（`sat_pass && full_pass`） |
| `normal_streak` | 本历元后连续正常历元数；攒够运行档 `isolation.min_epochs` 即在下一历元解算前解除 |
| `evaluable` | 本历元是否完成检验；基础解不可解/冗余不足为 false（数值记 0、计数清零） |

同一历元多颗隔离星都攒够历元时一并解除；刚解除的星不出现在当历元
`isolation_tests` 中，下一历元回到 `sat_results`。升级前写出的旧记录不含该字段，
加载后续跑保持原样、不回填。

## 会话状态响应中的统计

```json
{
  "stats": {
    "epochs": 24,
    "raim_available_epochs": 24,
    "alert_epochs": 0,
    "detections": 1,
    "alert_episodes": 0,
    "false_alarm_episodes": 0,
    "raim_availability": 1.0,
    "service_availability": 1.0
  },
  "state": {"isolated": {"4": {"id":4,"normal_streak":2,"since_epoch_seq":9}},
            "alert": {"active":false,"bad_streak":0,"good_streak":1}},
  "last_ts": 24
}
```

## 批量与逐历元一致性

对同一会话，把 N 个历元放进 `/epochs/batch` 与逐个 POST 到 `/epochs` 产生的**每一条记录、
隔离状态、告警状态与统计完全相同**；批量中任一历元非法会整段拒收，错误字段带历元下标
（如 `epochs[3].satellites[1].sigma`）。服务重启后对同一 `--data` 目录继续提交，
结果与不中断一致。
