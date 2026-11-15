# 复现探针（Repro Probe）

**状态**：已落地（2026-11-15）。工作流 `.github/workflows/repro-probe.yml`，脚本
`scripts/calendar-repro-probe.sh`、`scripts/browser-check-probe.sh`。

## 这份文档解决什么

仓库里有两条 P1 级的偶发问题，它们的共同形状是：**知道自己坏了，但复现不了，
于是只能「等复发观察」**。等是消极的——复发一次，证据大概率还是不够，下一次
仍然只能等。

复现探针把「等」换成「每天自动去撞」：在出问题的那个 runner 镜像上反复跑，
把每一轮的完整输出落盘；一旦复发，证据已经在那里了。

| 探针 | 盯着的现象 | 复发时要回答的问题 |
|------|-------------|-------------------|
| calendar | `internal/calendar` 的测试在 windows-latest 上偶发以 `exit status 0xc0000005`（访问违例）崩掉，runtime 一个字都没打印 | ① 崩不崩？② 完整 stderr 是什么？③ **崩溃是否总与 `-cover` 插值有关**？ |
| browser-check | browser-check 在 windows-latest 上偶发失败，**两个模式**：A = settle 超时后读到空 DOM；B = Chrome 进程根本没起来 | ① 十轮红几轮？② A 的 `settleDiagnosis` 的 `why:` 行是什么？③ B 时 **Chrome 自己的 stdout/stderr** 说了什么？ |

## 为什么不把它做成「自动重试」

**重试会洗绿。** 一次真崩溃被重试成绿，看起来像「偶发，实际没事」，而实际上
它只是被藏起来了——下一次复发时，我们依然没有证据。探针相反：它**失败就是
失败**，而且失败时留下的正是我们要的东西。

还有两条边界，刻意的：

- **探针不进合并门禁**。主线 required checks 不含本工作流。探针红了要报出来
  （它是一个红色告警），但不该挡住别人的合并——把「等复发观察」变成「不许合并」
  是本末倒置。
- **探针不改产品行为**。不加重试、不摘 `-cover`、不动断言预算、不换驱动版本。
  它只采集证据。

## 怎么跑

### 自动（每日）

`.github/workflows/repro-probe.yml` 每天 03:17 UTC 触发一次（刻意避开整点——
整点是所有定时任务的拥堵时刻）。也可以手动触发：

```bash
gh workflow run repro-probe.yml --ref master -f runs=10 -f calendar=true -f browser=true
```

`runs` 是每一项探针跑几轮（默认 5）。十轮比五轮贵一倍，但频率信息也准一倍。

### 本地

```bash
# calendar 探针（跨平台；只有 windows-latest 上才复现过，但本地跑也能给出
# 「今天这条腿稳不稳」的基线）
RUNS=5 PROBE_DIR=out/calendar scripts/calendar-repro-probe.sh

# browser-check 探针（需要 node、puppeteer-core 与一个能起来的浏览器）
cd web && RUNS=5 PROBE_DIR=../out/browser ../scripts/browser-check-probe.sh
```

## 产物怎么读

两个探针都写一份 `summary.txt`——**先看它**，再看细节。

### calendar

```
runs=5
plain_failures=0
cover_failures=1
crash_shape=access-violation-without-panic
failed_runs=cover-3
coverage_correlation=yes
```

- `plain_failures` / `cover_failures`：不带 / 带 `-cover` 各崩了几轮。
- `crash_shape`：`go-panic`（Go 层失败，runtime 打了 panic，好查）/
  `access-violation-without-panic`（进程被外部打断，没有 dump，**难查——
  正是当前这一类**）/ `none`。
- `coverage_correlation=yes` 表示只有带 `-cover` 的那次崩——那会直接缩小到
  覆盖率插值与退出时机的关系上。

每轮的完整输出在 `plain-round-N.log` / `cover-round-N.log`，环境信息在
`environment.txt`（Go 版本、runner 名字与 OS）。

### browser-check

```
runs=5
browser=chrome
failed_runs=run-2 run-5
launch_failures=1
settle_failures=1
assert_failures=0
env_failures=0
```

- `launch_failures`：模式 B（Chrome 没起来）。
- `settle_failures`：模式 A（页面没 settle）。
- `assert_failures`：控制台断言真的失败了——那不是这两个偶发问题，是**新问题**，
  要当回归处理。
- `env_failures`：探针自己的环境缺东西（没装 puppeteer-core、机器上没有浏览器）。
  单列一类，因为把它算成「断言失败」会把排查方向带偏。

每轮的日志在 `logs/run-N.log`，截图在 `shots/run-N/`。模式 B 的关键证据
（Chrome 自己的 stdout/stderr）在 `logs/run-N.log` 里——探针用
`BROWSER_CHECK_DUMPIO=1` 打开浏览器进程的输出采集，正常 CI 不开（免得日志变吵）。

## 结构守卫

`scripts/test_repro_probe.py`（随 Python 门禁跑）读**入库文本**，断言探针仍然
带着三条性质：

1. **重复跑**（一次 run 只能回答「这次红了」，频率才是这类问题的关键信息）；
2. **失败时照样上传产物**（`if: always()`——只在成功时上传等于永远没有证据）；
3. **不洗绿、也不进门禁**（探针红了以非零退出，但不在主线的 required checks 里）。

这三条都只写在文本里，没有别的机制保证它们不被改掉：有人删掉 `if: always()`、
把循环改成跑一次、或者把探针加进主线门禁，别的东西不会因此变红，而「证据悄悄
没了」正是这个工作流最该防的失败模式。

## 边界

- 探针**只在 windows-latest 上有意义**（两个现象都只在那里出现过）。别处跑它
  给的是「今天没复现」，不是「问题不存在」。
- 探针红了**不等于**根因找到了：它给的是证据。根因仍要人来读证据。
- 探针占用 runner 时间（每日两个 job）。这是为「把等换成撞」付的账，账记在这里。
