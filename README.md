# codex-lazy-runner

Codex 的 5 小时额度用完时，任务会被中断。lazy-runner 会一直等到额度重置（或者你用了额度卡），然后自动用 `codex exec resume` 接着原会话继续跑，直到任务完成。

- 额度信息通过 `codex app-server` 的 `account/rateLimits/read` 查询，**不消耗 token**。查询失败时，改用会话日志中的 `rate_limits` 和报错里的 "try again at …" 估算重置时间。
- 等待期间默认每 5 分钟查一次额度。无论是到点自然重置，还是你在网页或 App 上用了额度卡，都能及时发现并马上续跑。
- 周额度用完时只发系统通知，不会自动等待。
- 不改动你的 codex 配置，权限和模型都沿用 `~/.codex/config.toml`。
- 支持 macOS 和 Linux，只依赖 Go 标准库。

## 安装

```sh
go build -o bin/lazy-runner ./cmd/lazy-runner
# 可选：放进 PATH
cp bin/lazy-runner /usr/local/bin/
```

## 用法

### 新建任务

```sh
lazy-runner run "把 parser 重构成流式解析并补全测试"
lazy-runner run --cd ~/proj --prompt-file task.md
# -- 之后的参数会原样传给 codex exec
lazy-runner run "修复 CI" -- -m gpt-5.5 -s workspace-write
```

### 接管已经中断的会话

在 TUI 或 Codex Desktop 里跑到一半因额度中断的会话，也可以接管：

```sh
lazy-runner resume --list            # 列出最近 7 天因额度中断的会话
lazy-runner resume --last            # 接管最近一个
lazy-runner resume 01a1615c-0f92-…   # 按会话 ID、会话名或 lazy-runner 任务 ID 接管
```

> 如果会话还在 Codex Desktop 或 TUI 里开着，codex 会持有这个会话的写锁。lazy-runner 检测到后会发通知并等待，你关闭该会话（或退出应用）后，它会自动续跑。

### 额度卡

```sh
lazy-runner credits list             # 查看额度和额度卡
lazy-runner credits use              # 用一张卡（需要确认，--yes 跳过确认）
```

用卡成功后，会自动唤醒所有正在等待的任务。如果你是在网页或 App 上用的卡，等待中的任务最迟在下一次轮询时（默认 5 分钟内）发现额度已恢复；也可以手动唤醒：

```sh
lazy-runner wake <任务ID>            # 立即检查额度，能用就续跑
lazy-runner wake --all
lazy-runner wake --force <任务ID>    # 不检查额度，直接续跑
```

也可以让它自动用卡（默认关闭）：

```sh
# 额度耗尽、且距离自然重置还超过 1 小时，才自动用卡
lazy-runner run --auto-credit --auto-credit-min-wait 1h "…"
```

### 查看和管理任务

```sh
lazy-runner status          # 当前额度、额度卡、任务列表
lazy-runner cancel <任务ID>  # 取消任务（正在运行的 codex 也会被中断）
```

## 常用选项（run 和 resume 通用）

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--continue-prompt` | 内置中文提示 | 续跑时发给 codex 的消息 |
| `--poll` | `5m` | 等待期间多久查一次额度 |
| `--buffer` | `2m` | 到达重置时间后再多等多久 |
| `--max-resumes` | `20` | 最多续跑多少次 |
| `--max-errors` | `3` | 遇到额度以外的错误时，最多连续重试几次（间隔递增） |
| `--auto-credit` / `--auto-credit-min-wait` | 关 / `1h` | 自动用卡 |
| `--keep-awake` | 关 | 运行期间阻止系统空闲睡眠（macOS 的 caffeinate） |
| `--no-notify` | 关 | 不发系统通知 |

等待动辄几个小时，建议放在 tmux 或 screen 里运行，或者用 `nohup lazy-runner resume --last &`。如果 Mac 合盖进入睡眠，唤醒后会立即按真实时间重新判断，不会因为睡眠而晚跑。

## 退出码

| 码 | 含义 |
|---|---|
| 0 | 任务完成 |
| 1 | 失败（codex 无法启动会话、连续异常、超过续跑次数） |
| 3 | 周额度已用完，任务已暂停 |
| 4 | 已取消 |
| 130 | 被 Ctrl+C 或 SIGTERM/SIGHUP 中断（可用 `resume <任务ID>` 继续） |

## 数据位置

- `~/.codex-lazy-runner/state.json`：任务状态（可用 `LAZY_RUNNER_HOME` 修改目录）
- `~/.codex-lazy-runner/logs/<任务ID>/NN.jsonl`：每次运行 codex 的原始事件；同目录下的 `NN.stderr.log` 是它的 stderr

## 开发

```sh
go test -race ./...
```

`internal/runner/testdata/fakecodex` 是一个假的 codex。端到端测试用它模拟"额度用完 → 等待 → 用卡/唤醒 → 续跑 → 完成"等流程，不会动到真实额度。
