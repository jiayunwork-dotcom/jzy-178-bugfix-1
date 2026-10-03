# 升级前会话夹具（只读）

本目录由升级前格式的会话文件构成，供 `TestPreUpgradeSessionResume` 使用，
**不要手工编辑或用新版程序重新生成**。

- `profiles/iso4-upgrade.json`：测试用运行档（Pfa=1e-3、HAL 极大、min_epochs=4）。
- `sessions/dualfault.json`：10 星、σ=1 m。PRN3 在历元 5..10 加 80 m 偏差，
  PRN6 自历元 8 起一直加 80 m；只提交到历元 12，此时 PRN3 正在隔离且
  normal_streak=2（历元 11、12 已连续正常），PRN6 在历元 12 刚被唯一排除。
  记录只跑到 12，历元 13 起由测试加载后续跑。
- `sessions/singlefault.json`：单星（PRN3）隔离中的会话，同样只跑到历元 12。

两份会话的逐历元记录均不含新版才引入的 `isolation_tests` 字段（夹具生成时
按旧版 JSON 形态剥除），用于验证：

1. 升级后能直接加载并继续提交；
2. 隔离集合、normal_streak、since_epoch_seq 沿用，恢复时序不重置；
3. 已有历史记录原样保留（JSON 字节不改写、不回填新字段）；
4. 续跑的新历元与一口气跑完逐项相等。

几何/噪声固定种子：星座 `sim.EvenSky(rec, 10, 15, 5)`，噪声种子 20260930
（singlefault 为 777）。
