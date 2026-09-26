# 原始测量汇总

本表只展示各阶段实测，不自动将短时峰值认定为持续容量。

| 阶段 | 目标 RPS | 秒 | 实际成功 RPS | 错误/丢弃 | P95/P99 ms | Outbox 起始→峰值→结束 | 命令完成 P95 ms |
| --- | ---: | ---: | ---: | --- | --- | --- | ---: |
| [warmup-latest-10](warmup-latest-10.json) | 10 | 30 | 10.0 | 0/0 | 9.9/25.6 | 0→0→0 | 0.0 |
| [screen-detail-hot-20](screen-detail-hot-20.json) | 20 | 30 | 20.0 | 0/0 | 2.7/3.4 | 0→0→0 | 0.0 |
| [screen-detail-hot-100](screen-detail-hot-100.json) | 100 | 30 | 100.0 | 0/0 | 2.2/2.7 | 0→0→0 | 0.0 |
| [screen-detail-hot-300](screen-detail-hot-300.json) | 300 | 30 | 300.0 | 0/0 | 1.8/2.3 | 0→0→0 | 0.0 |
| [screen-latest-20](screen-latest-20.json) | 20 | 30 | 20.0 | 0/0 | 8.9/11.6 | 0→0→0 | 0.0 |
| [screen-latest-100](screen-latest-100.json) | 100 | 30 | 100.0 | 0/0 | 3.8/8.6 | 0→0→0 | 0.0 |
| [screen-latest-300](screen-latest-300.json) | 300 | 30 | 300.0 | 0/0 | 3.4/7.9 | 0→0→0 | 0.0 |
| [screen-detail-random-20](screen-detail-random-20.json) | 20 | 30 | 20.0 | 0/0 | 6.9/9.2 | 0→0→0 | 0.0 |
| [screen-detail-random-100](screen-detail-random-100.json) | 100 | 30 | 100.0 | 0/0 | 6.5/8.3 | 0→0→0 | 0.0 |
| [screen-detail-random-300](screen-detail-random-300.json) | 300 | 30 | 298.5 | 43/0 | 16.8/56.2 | 0→0→0 | 0.0 |
| [screen-likes-20](screen-likes-20.json) | 20 | 30 | 20.0 | 0/0 | 9.6/11.4 | 0→0→0 | 0.0 |
| [screen-likes-100](screen-likes-100.json) | 100 | 30 | 100.0 | 0/0 | 9.1/13.3 | 0→0→0 | 0.0 |
| [screen-likes-300](screen-likes-300.json) | 300 | 30 | 300.0 | 0/0 | 12.3/138.6 | 0→0→0 | 0.0 |
| [screen-hot-20](screen-hot-20.json) | 20 | 30 | 0.0 | 600/0 | 3459.4/3603.2 | 0→0→0 | 0.0 |
| [screen-write-20](screen-write-20.json) | 20 | 30 | 20.0 | 0/0 | 101.2/103.4 | 0→219→0 | 61.0 |
| [screen-write-100](screen-write-100.json) | 100 | 30 | 96.5 | 0/0 | 3075.3/3725.2 | 0→5300→4321 | 1862.0 |
| [screen-write-300](screen-write-300.json) | 300 | 30 | 126.0 | 5/4703 | 6413.8/8087.4 | 4187→11974→10994 | 3814.0 |
| [screen-async-20](screen-async-20.json) | 20 | 30 | 20.0 | 0/0 | 34.9/36.5 | 10954→11154→10174 | 37.0 |
| [recovery-read-detail-hot-1000](recovery-read-detail-hot-1000.json) | 1000 | 30 | 1000.0 | 0/0 | 1.7/2.3 | 9093→9093→7101 | 0.0 |
| [recovery-read-detail-hot-2000](recovery-read-detail-hot-2000.json) | 2000 | 30 | 1999.9 | 0/0 | 2.9/10.5 | 6954→6954→4974 | 0.0 |
| [recovery-read-latest-1000](recovery-read-latest-1000.json) | 1000 | 30 | 998.2 | 0/0 | 37.2/131.4 | 4834→4834→2854 | 0.0 |
| [recovery-read-latest-2000](recovery-read-latest-2000.json) | 2000 | 30 | 1447.9 | 0/16154 | 728.3/1378.4 | 2714→2714→714 | 0.0 |
| [recovery-read-likes-1000](recovery-read-likes-1000.json) | 1000 | 30 | 439.8 | 124/16172 | 2641.2/3722.7 | 674→674→0 | 0.0 |
| [steady-async-10](steady-async-10.json) | 10 | 120 | 10.0 | 0/0 | 52.3/54.4 | 0→10→0 | 54.0 |
| [steady-mixed-100](steady-mixed-100.json) | 100 | 600 | 100.0 | 0/0 | 55.0/65.6 | 0→12→0 | 29.0 |
