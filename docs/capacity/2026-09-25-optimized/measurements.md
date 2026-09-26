# 原始测量汇总

本表只展示各阶段实测，不自动将短时峰值认定为持续容量。

| 阶段 | 目标 RPS | 秒 | 实际成功 RPS | 错误/丢弃 | P95/P99 ms | Outbox 起始→峰值→结束 | 命令完成 P95 ms |
| --- | ---: | ---: | ---: | --- | --- | --- | ---: |
| [optimized-hot-20](optimized-hot-20.json) | 20 | 90 | 20.0 | 0/0 | 9.9/27.0 | 0→0→0 | 0.0 |
| [optimized-hot-100](optimized-hot-100.json) | 100 | 90 | 100.0 | 0/0 | 9.4/659.0 | 0→0→0 | 0.0 |
| [optimized-write-20](optimized-write-20.json) | 20 | 90 | 20.0 | 0/0 | 99.8/103.1 | 0→4→0 | 57.0 |
| [optimized-write-100](optimized-write-100.json) | 100 | 90 | 99.0 | 0/0 | 1099.7/1299.2 | 0→45→0 | 894.0 |
| [final-hot-100](final-hot-100.json) | 100 | 180 | 100.0 | 0/0 | 8.3/10.1 | 0→0→0 | 0.0 |
| [final-write-100](final-write-100.json) | 100 | 180 | 99.4 | 0/0 | 1565.7/2017.3 | 0→43→0 | 1184.0 |
| [final-async-100](final-async-100.json) | 100 | 180 | 100.0 | 0/0 | 78.1/90.8 | 0→16→0 | 9819.0 |
| [refined-write-100](refined-write-100.json) | 100 | 180 | 99.6 | 0/0 | 938.2/1271.8 | 0→20→0 | 815.0 |
| [refined-async-100](refined-async-100.json) | 100 | 180 | 100.0 | 0/0 | 77.5/93.3 | 0→12→0 | 5253.0 |
| [stable-async-50](stable-async-50.json) | 50 | 180 | 50.0 | 0/0 | 63.2/76.2 | 0→6→0 | 53.0 |
| [refined-hot-100](refined-hot-100.json) | 100 | 90 | 100.0 | 0/0 | 8.5/11.3 | 0→0→0 | 0.0 |
