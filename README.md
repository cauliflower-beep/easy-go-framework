# easy-go-framework

可复用的 Go 基础库，为服务提供配置加载、日志、HTTP 服务与客户端、PostgreSQL、Redis、对象存储、异步任务、定时调度和终端显示能力。

框架作为依赖参与应用编译，不单独启动服务，也不包含具体业务逻辑。

## 从完整示例开始

[examples/server/main.go](examples/server/main.go) 是可编译运行的应用组合根，演示严格配置加载、模块日志、HTTP 路由、可选 PostgreSQL、就绪检查和信号退出。默认不启用数据库，不需要外部服务：

```powershell
go run ./examples/server
```

访问 `http://127.0.0.1:8080/example`、`/healthz` 和 `/readyz`，按 Ctrl+C 退出。指定配置文件时运行：

```powershell
go run ./examples/server -config ./examples/server/config.yaml
```

启用数据库时先准备专用数据库和账号，设置 `EXAMPLE_POSTGRES_ENABLED=true`，通过 `EXAMPLE_POSTGRES_CONNECTION_PASSWORD` 提供密码，再使用示例配置启动。`/database-time` 演示有期限的 GORM 查询；数据库探针只影响就绪，不影响存活。示例 YAML 显式使用本地明文连接，生产应改为 `verify-full`，按证书实际主机名和可信 CA 配置连接。

## 默认配置与严格加载

各基础设施组件均提供 `DefaultOptions()`：HTTP 服务/客户端、日志、PostgreSQL、Redis、对象存储、调度器和任务队列。默认值只提供技术参数；应用身份、数据库名/账号、云凭证等由应用提供。构造函数始终 Validate，不隐式补齐传入配置中的零值。HTTP 服务默认仅监听 `127.0.0.1:8080`；日志默认输出控制台；PostgreSQL 默认 `verify-full`。

应用可直接组合结构化默认值，无需重复维护一份叶子字段字符串映射：

```go
type Settings struct {
    HTTP httpserver.Options `mapstructure:"http"`
    Log  logging.Options    `mapstructure:"log"`
}
defaults := Settings{HTTP: httpserver.DefaultOptions(), Log: logging.DefaultOptions()}
var settings Settings
err := config.Load(path, &settings, config.Options{
    EnvPrefix: "MYAPP",
    DefaultsFrom: defaults,
})
// 加载成功后，应用仍须显式调用 settings.HTTP.Validate()/settings.Log.Validate()。
```

优先级为 **环境变量 > YAML > Options.Defaults 显式键 > DefaultsFrom 结构默认值**。`DefaultsFrom` 接收与目标同类型的结构体或非 nil 指针；原有 `Defaults: map[string]any{...}` 继续可用。目标原有值不会自动充当默认值。失败不修改目标，也不修改传入默认配置。`MYAPP_HTTP_PORT` 对应 `http.port`；空前缀禁用环境覆盖。时长使用 `5s` 等带单位文本，显式空环境变量交由类型检查和应用校验，YAML null、未知字段、类型错误和多文档被拒绝。

每次加载都会复制结构化默认值和 `Defaults` 中的可变配置数据，包括复合切片、map、数组和指针，加载结果不与默认值或其他加载结果共享这些引用。默认值应是普通配置数据；参与配置复制的函数、channel、unsafe pointer 以及循环或超过 64 层的嵌套返回错误，失败仍不修改目标。并发加载可以复用同一份只读默认值，调用方不能在加载期间并发改写它。

被 `mapstructure:"-"` 忽略或未导出的字段不会被递归复制：只允许保留纯值数据和 nil 引用。即使这些字段藏在切片、map、数组、指针或 interface 的嵌套结构内，含非 nil 可变引用或运行期资源时仍会拒绝默认配置，并报告字段名，不输出默认值内容；不会静默清空字段或浅拷贝共享对象。含私有可变引用的封装类型也受此限制，应用应使用普通配置数据，在加载完成后再装配缓存、回调和其他运行期状态。

日志使用 `logging.New(identity, options, output, diagnostics)`，通过 `Manager.Module(name)` 取得模块日志；输出 Writer 归应用所有，管理器只关闭自己的文件。内部依赖故障使用 `logging.ErrorFields(err)` 记录受控诊断，避免直接打印解包后的驱动错误、完整配置、请求和响应体。HTTP panic 只记录固定分类、请求 ID 和堆栈，不格式化原始 panic 值。

## 生命周期契约

组件不接管进程信号，关闭由应用组合根协调。对提供两种关闭方式的资源，正常退出使用 `Shutdown(ctx)` 停止准入、等待在途工作和诊断结束；`Close()` 用于立即取消的资源兜底。等待上限通常取调用方 Context 与组件 `ShutdownTimeout` 的较短者。

| 组件 | Shutdown(ctx) | Close() |
| --- | --- | --- |
| HTTP 服务 | 停止监听/就绪及准入，等待 Handler 和日志 | 立即取消请求并关闭连接，再有界等待 |
| PostgreSQL | 停止新会话、事务及 Ping，等待回调完成后关闭池 | 保持原有立即取消语义，再有界等待回调和池回收 |
| Redis | 停止新回调/Ping，等待回调后关闭池 | 立即取消并关闭连接，再有界等待 |
| HTTP 客户端 | 停止新请求，等待响应体消费/关闭和日志 | 立即取消在途请求，再有界等待 |
| 对象存储 | 停止新操作，等待 SDK 调用、下载流及日志 | 立即取消在途操作，再有界等待 |
| 任务提交端 | 停止新操作，等待提交/查询和日志 | 立即取消操作、关闭连接，再有界等待 |
| 调度器 | 停止新触发，等待函数和日志 | 立即取消函数 Context，再有界等待 |
| 任务 Worker | 停止取任务，交给引擎完成执行或停机重新入队 | **与 Shutdown 相同**，保留引擎租约/重新入队收尾 |
| 日志 Manager | 无此方法；不拥有业务执行 Context | 在生产者停止写入后调用，刷新并回收日志文件 |

Worker 的例外是任务引擎的必要语义，不提供绕过收尾直接断开 Redis 的强制销毁。日志是同步输出资源，不人为引入任务排空接口。

HTTP 服务和 PostgreSQL 均支持重复、并发关闭；任一调用超时/取消或显式 Close 都会取消全局在途工作。**超时返回不证明函数已经退出**，也不额外追加隐藏的等待时间；应用须保留未退出函数使用的依赖和日志，可再次调用 Shutdown/Close 等待。已完成关闭时，重复调用返回实际回收结果，不因新传入的 Context 已取消而改成失败。回调/Handler 内不能同步等待自身所属组件退出。

典型退出顺序：请求入口和调度来源 → 任务提交端 → Worker → 业务依赖 → 日志。先确保所有依赖生产者停止，再释放共享客户端。示例在 HTTP 或数据库排空失败时保留后续依赖，将错误交回进程入口，避免误把超时当成已经安全收尾。

HTTP 服务默认面向有界 API，请求体全量检查受 `BodyLimit` 限制，不作为大文件流式上传入口；文件可使用对象存储签名直传。数据库会话、事务和 Redis 命令对象只在回调内使用，必须返回最终操作错误。长期阻塞 Redis 工作使用独立连接池及 `WithClientTimeout`；长数据库操作使用单独配置的 Database，注意 QueryTimeout 同时设置服务端 statement_timeout，不能仅延长父 Context 绕过。当前不为 Watch、订阅或跨存储事务提供隐式兼容路径。

## 定时调度

`scheduler` 基于 robfig/cron/v3 在当前进程按时间调用处理函数，**不依赖 taskqueue、Asynq 或 Redis**。支持标准五段 cron（分/时/日/月/周）、`@daily`、`@hourly` 等描述符，以及整秒的 `@every 10m` 间隔；不支持六段秒字段或年份字段。适合不需要持久化队列的定期检查、临时文件清理等工作，具体操作由应用传入。

```go
options := scheduler.DefaultOptions()
options.TimeZone = "Asia/Shanghai"
timer, err := scheduler.New(options, logger)
if err != nil {
    return err
}
defer timer.Close()
err = timer.Register(scheduler.Plan{
    Name: "example-daily", Spec: "0 3 * * *", Timeout: time.Minute,
}, func(ctx context.Context) error {
    return cleanupTemporaryFiles(ctx) // 应用自己的函数，应遵守 ctx 取消。
})
if err != nil {
    return err
}
if err := timer.Start(); err != nil {
    return err
}
entry, err := timer.Get("example-daily")
```

应用可将 `scheduler.Options` 和 `[]scheduler.Plan` 组合进自身配置，再显式为各计划绑定处理函数；组件不自动读取配置文件、解析函数名、连接服务或接管进程信号。

```yaml
scheduler:
  time_zone: Asia/Shanghai
  run_timeout: 1m
  shutdown_timeout: 5s
plans:
  - name: example-daily
    spec: "0 3 * * *"
    timeout: 2m
```

`New(options, logger)` 无需任务客户端；`Register(plan, handler)` 的处理函数为 `func(context.Context) error`。全局 `run_timeout` 默认 1 分钟，Plan 的 `timeout=0` 继承该值、正值覆盖，不要求整秒。任务类型、载荷和重试字段属于下面的可选队列适配配置；直接执行与持久化队列由应用明确选择，不自动降级。

默认时区为 UTC，Plan 的 `time_zone` 留空继承全局值。要求明确的 IANA 时区，拒绝隐式 `Local` 和表达式内的 TZ/CRON_TZ 前缀。程序内嵌时区数据，Windows 开发与 Linux 部署不要求预装时区数据库。夏令时跳过不存在的本地时间，回拨时重复的本地时间可能触发两次。`@every` 按库规则对齐到整秒，不作为高精度计时器。

处理函数在 cron 创建的 goroutine 中执行；同一计划的上一次函数尚未返回时，跳过本次触发并更新 `LastSkippedAt`，不积累等待队列。不同计划可并发执行。函数应完成本次工作后再返回；自行启动 goroutine 后立即返回会使完成状态、重叠控制和关闭等待失去意义。超时通过 Context 协作取消，不能强行终止 Go 函数；超时后函数尚未返回时仍保持 Running 并跳过重叠触发，函数最终返回 nil 也会记录为期限失败。

`Entries()` 按名称排序；`Get(name)` 返回单计划快照，不存在时为 `ErrNotFound`。快照包含 `NextRun`、`LastStartedAt`、`LastFinishedAt`、`LastRunID`、`LastOutcome`、`LastFailure`、`LastSkippedAt` 和 `Running`；`LastOutcome` 为 never/running/succeeded/failed。`LastFinishedAt` 在本次执行期间为空，函数返回后填写；`Running` 直到日志收尾后才清除。每次调用生成新的 UUID，在函数内用 `scheduler.RunID(ctx)` 获取。快照不包含函数参数或返回数据，只保存在当前进程，重启后重置。

Start 只启动未来触发，不立即执行计划。所有计划在启动前注册，修改需重建调度器。首版每组计划只启用一个调度实例，多进程会各自触发；不提供分布式选主、跨进程去重、失败自动重试或持久化调度历史。停机、休眠及系统时钟变化期间错过的触发不自动补跑。进程内函数在进程崩溃后不会自动恢复，需要恢复和重试时使用持久化队列组合。

`Shutdown(ctx)` 停止新触发，等待在途函数和日志收尾，上限取调用方期限与 `ShutdownTimeout` 的较短者，超时后取消在途 Context。`Close()` 立即停止并取消，再有界等待。支持重复和并发调用，关闭对象不可重启；超时返回不代表不响应取消的函数已退出，应保留其依赖并可再次关闭等待。组件捕获函数 panic，日志记录计划名、运行 ID、耗时和安全失败分类，不记录原始 panic 值或任意处理函数错误文本；详细业务错误由应用在处理函数内部按自身规则记录。

### 可选：通过持久化任务队列执行

仅需要 Redis 队列的项目导入 `github.com/cauliflower-beep/easy-go-framework/scheduler/queue`（下例别名 `queueschedule`）。适配层将定时函数绑定为提交任务，调度核心和直接执行路径不引入队列依赖。

```go
err = queueschedule.Register(timer, taskClient, queueschedule.Plan{
    Schedule: scheduler.Plan{
        Name: "example-queued", Spec: "0 3 * * *", Timeout: 5 * time.Second,
    },
    TaskType: "example.echo", Payload: `{"message":"scheduled hello"}`,
    TaskTimeout: time.Minute, MaxRetry: 0,
}) // 在 timer.Start() 之前调用；taskClient 使用既有 taskqueue 配置构造。
```

队列计划可用 `[]queue.Plan` 承接以下 YAML；处理函数计划与队列计划由应用分别注册，可共用一个调度器：

```yaml
queue_plans:
  - schedule:
      name: example-queued
      spec: "0 3 * * *"
      timeout: 5s
    task_type: example.echo
    payload: '{"message":"scheduled hello"}'
    task_timeout: 1m
    max_retry: 0
```

注册时通过 `taskqueue.Client.ValidateSubmission` 校验任务类型、JSON、大小和执行期限，不连接 Redis。Payload 是 JSON 文本，不支持模板；空值采用 JSON null。`TaskTimeout=0` 继承队列默认值，正值使用整秒；MaxRetry 显式配置额外尝试次数。Worker 必须注册对应 TaskType，适配层不核实远程处理函数。

`Schedule.Timeout` 约束入队函数，实际还受任务客户端 `OperationTimeout` 限制；`TaskTimeout` 约束入队后的业务执行。`Entry.LastRunID` 同时是队列任务 ID，可通过 `taskClient.Get(ctx, entry.LastRunID)` 查询真实状态和结果。**调度 succeeded 仅表示提交成功，不代表 Worker 已完成业务。** 入队超时或失败仍可能已经提交，可用该 ID 核实；适配层不自动补发。

重叠控制只覆盖入队函数，不能防止先前的 Worker 任务仍在执行时下一周期再次提交。调度器关闭不等待已入队任务执行，也不关闭借用的 Client。退出顺序为调度器 → 任务提交端 → Worker → 业务依赖 → 日志；队列仍遵守至少一次执行和业务幂等边界。适配 Plan 的常见格式化隐藏 Payload，但配置 JSON 序列化仍保留它，不应整体写日志。

独立测试覆盖无 Redis 的真实 cron 函数调用、时区/夏令时、YAML 组合、函数期限、重叠跳过、并发关闭、异常隔离，以及可选适配层通过进程内 Redis 协议服务完成的入队 → Worker → 结果查询。不连接开发环境，不代表真实 Redis 持久化或 Linux 运行已验收：

```powershell
go test -count=1 -race ./scheduler/... ./taskqueue
```

实现依据：[robfig/cron v3 文档](https://pkg.go.dev/github.com/robfig/cron/v3)。

## 异步任务

`taskqueue` 基于 Asynq v0.26.0 提供 Redis 持久化队列，包含即时/延迟提交、业务处理函数注册、后台消费、状态查询及小型 JSON 结果保留。它不启动业务 HTTP 接口，也不定义业务任务表或工作流。周期调度通过上面的可选 `scheduler/queue` 适配层组合，不开放任务管理后台或任意 Asynq 管理命令。

提交端 `Client` 和消费端 `Worker` 独立构造，可放在同一进程，也可由应用拆成独立程序运行，不要求新增消息服务器或容器。使用前准备专用 Redis 数据库及任务账号，区分开发和生产队列：

```go
options := taskqueue.DefaultOptions()
options.Queue = "development-tasks"
options.Redis.Host = redisHost
options.Redis.User = redisUser
options.Redis.Password = redisPassword
options.Redis.Database = taskDatabase
options.Redis.TLS = redisTLS // 本地无 TLS 时显式 false；默认启用证书与主机名校验

client, err := taskqueue.NewClient(options, logger)
if err != nil {
    return err
}
defer client.Close()
worker, err := taskqueue.NewWorker(options, logger)
if err != nil {
    return err
}
defer worker.Close()
err = worker.Register("example.echo", func(ctx context.Context, task taskqueue.Task) (json.RawMessage, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }
    return task.Payload, nil
})
if err != nil {
    return err
}
if err := worker.Start(ctx); err != nil {
    return err
}
info, err := client.Enqueue(ctx, taskqueue.Submission{
    Type: "example.echo", Payload: json.RawMessage(`{"message":"hello"}`),
    MaxRetry: 0, // 普通失败后不自动重试；业务确认允许时显式设置额外尝试次数
})
if err != nil {
    return err
}
// 在后续请求中查询；首次返回可能仍为 pending/active。
status, err := client.Get(ctx, info.ID)
```

`NewClient`/`NewWorker` 仅本地装配。`Client.Ping(ctx)` 可检查连通性；`Worker.Start(ctx)` 在开始取任务前检查连接和认证，失败可重试启动。Start 的 Context 只控制启动检查，Enqueue 的 Context 只控制提交；已接受的后台任务不随 HTTP 请求结束而取消。应用自行处理信号并调用关闭方法。

`Submission.ID` 为空时生成 UUID，也可预先提供规范的小写非零 UUID。提交超时/断连可能已经入队，调用方可用预先指定的 ID 查询并核实；重复 ID 在原任务记录仍存在时返回 `ErrIDConflict`，不等于原任务已成功。记录过期后 ID 可再次使用，不能以此替代业务幂等。`Delay` 默认为零，正值为整秒；SDK 按 Unix 秒保存，封装向上取整避免提前执行，实际启动受轮询及队列占用影响。

`TaskTimeout` 默认 5 分钟，可用每次提交的正 `Timeout` 覆盖，均为整秒；超时通过 Context 协作取消，无法强行终止不响应取消的 Go 函数、外部进程或外部服务。`Concurrency` 默认 4，限制此 Worker 的 SDK 在途处理槽位；若业务函数忽略取消，已超时但仍运行的函数可能与后续尝试重叠，不能把它解释为所有业务副作用的硬并发上限。

`MaxRetry` 为额外尝试次数，默认 0。显式开启后，普通失败/panic 采用固定 `RetryDelay`（默认 30 秒）延迟重试；处理函数返回 `taskqueue.Permanent(err)` 可跳过后续自动重试。未知任务类型、畸形载荷、不合法/过大结果直接进入 failed。失败记录使用稳定分类，不保存任意异常或 panic 文本。停止或崩溃恢复可能重新投递任务，即使 MaxRetry=0 也不承诺只执行一次；`Task.Attempt` 是引擎重试序号，不是全局唯一执行次数。

任务载荷和结果均为 UTF-8 JSON，各默认最多 64 KiB。载荷会存入 Redis，结果由处理函数返回后写入 Redis；日志和失败记录不展开载荷、结果、凭证或任意异常文本，但任务数据本身并未加密。文件和大型产物应存对象存储，任务只传引用。`Task.Payload` 属于本次尝试；处理函数不得异步保留后继续写结果。`Info.Result` 只在 completed 时返回，Info 不直接充当面向用户的响应，业务自行校验任务归属与结果访问权限。

`Get` 返回 pending、scheduled、active、retry、failed 或 completed。成功结果默认保留 24 小时，随后按引擎清理周期删除；失败任务按 Asynq 的归档策略保留，并非永久审计记录。`LastFailure` 是最近一次失败的安全分类，成功重试后也可能存在；它不是最终业务结论。记录不存在返回 `ErrNotFound`，不能证明任务从未执行。永久业务结果和进度模型由应用维护。

任务及结果落到 Redis，不代表机器断电后一定不丢。持久化程度取决于 Redis AOF/RDB、刷盘与备份配置，队列实例不应允许内存淘汰任务键。Asynq 会使用 Lua、集合、排序集合及 Pub/Sub 等命令，缓存测试账号未必具有所需权限；服务账号需按实际功能配置。Queue 用于队列分流，不能充当租户安全隔离或 Redis 键前缀隔离；不同环境宜使用独立实例或数据库，框架不会修改 Redis 持久化、淘汰或权限设置。

组件复用 `redis.Options.DriverOptions()` 的已校验连接/TLS/连接池配置，分别持有独立连接池，不把原 Redis Client 带出回调。驱动命令自动重试关闭，队列引擎仍有维护租约和状态同步等恢复行为。`OperationTimeout` 限制提交/查询/Ping，查询中 Asynq Inspector 的底层命令也使用同一个 Context。`Redis.CommandTimeout`、`Redis.ShutdownTimeout`、`Redis.SlowThreshold` 属于通用 Redis 回调客户端，在此不代替队列自己的期限或日志设置。应用组合配置时显式提供加载器 Defaults 并执行 Validate。

关闭顺序由应用决定：先停止新任务来源，再关闭提交端，随后关闭消费者，最后关闭其仍需使用的数据库、对象存储与日志。`Client.Shutdown(ctx)` 拒绝新提交/查询并等待在途请求，期限到后取消请求并关闭连接；`Client.Close()` 立即取消、有界等待。

`Worker.Shutdown(ctx)` 开始停止取任务，允许正在执行的函数完成；SDK 在配置的 `ShutdownTimeout` 后取消未完成执行并尝试重新入队。调用方 Context 限制等待时间，提前返回不会破坏 SDK 的重新入队收尾，也不会直接把停机取消解释为业务失败。`Worker.Close()` 同样按配置有界等待。超时意味着后台清理或不响应取消的用户函数可能仍在运行，可以再次调用 Shutdown 等待；不能在任务处理函数中同步等待自身 Worker 退出，不能在返回超时后就假定依赖和日志已可安全销毁。一个 Worker 只成功启动一次，注册必须在启动前完成。

执行日志包含任务 ID、类型、尝试次数与安全失败分类。引擎原始格式化日志可能带有私密内容，因此只输出固定引擎事件，具体任务失败由结构化回调记录；业务细节由处理函数使用请求关联 Context 自行安全记录。`SafeError` 保留错误链与 `ErrNotFound`、`ErrIDConflict`、`ErrClosed`、Context 及 Redis 认证/权限错误分类；禁止直接打印解包后的错误。常见格式化会隐藏任务载荷和结果；JSON 序列化有意保留数据以供应用使用，不能整体写日志。

独立测试使用 Asynq 与进程内 miniredis 协议/Lua 服务，验证队列逻辑和消费者重建，不连接开发环境，也不能据此宣称真实 Redis 的落盘或进程崩溃恢复已经验收：

```powershell
go test -count=1 -race ./taskqueue ./redis
```

真实 Redis 测试默认跳过。以后需要时，显式设置 `TASKQUEUE_TEST_INTEGRATION=1`，并提供 `TASKQUEUE_TEST_HOST`、`TASKQUEUE_TEST_PORT`、`TASKQUEUE_TEST_DATABASE`、`TASKQUEUE_TEST_TLS` 和专用 `TASKQUEUE_TEST_USER`/`TASKQUEUE_TEST_PASSWORD`；可选 `TASKQUEUE_TEST_TLS_ROOT_CERT`。测试仅操作自己生成的随机队列及任务，不执行清库命令。该账号需允许任务引擎的全套命令和相应键访问。

```powershell
go test -count=1 -run '^TestIntegrationTask' -v ./taskqueue
```

实现依据：[Asynq 官方说明](https://github.com/hibiken/asynq/tree/v0.26.0)。采用精确版本，后续升级需复核 Inspector 的 Context 适配、队列协议与停机行为；Asynq 尚为 v0.x，不承诺跨版本 API/存储兼容。任务接受与 PostgreSQL 业务提交不是同一个事务，跨存储一致性及重复副作用由应用决定，不能把“成功入队”解释为“业务已完成”。

## 对象存储

`objectstore` 提供统一的对象上传、流式下载、元信息查询、删除、bucket 访问检查，以及临时上传/下载链接。开发环境使用本地 S3 服务（例如 MinIO），生产可切换阿里云 OSS。`s3` 使用 AWS Go SDK v2，`oss` 使用阿里 OSS Go SDK v2；业务代码使用相同接口，提供方差异保留在适配器中，不依赖 OSS 完整兼容 S3 的假设。

以下地址和 bucket 均为示例，需提前部署服务并创建私有测试 bucket；组件不会创建 bucket 或修改 ACL：

```go
options := objectstore.DefaultOptions()
options.Provider = objectstore.ProviderS3
options.Endpoint = "http://192.168.6.85:9000" // 示例，不表示该主机已经部署对象存储
options.Region = "us-east-1"                 // 与本地服务实际配置一致
options.Bucket = "development-files"
options.UsePathStyle = true
options.AllowInsecureHTTP = true // 仅供明确允许 HTTP 的开发环境
options.AccessKeyID = accessKeyID
options.AccessKeySecret = accessKeySecret

store, err := objectstore.New(options, logger)
if err != nil {
    return err
}
defer store.Close()
if err := store.Ping(ctx); err != nil {
    return err
}

file, err := os.Open("example.png")
if err != nil {
    return err
}
defer file.Close()
_, err = store.Put(ctx, objectstore.PutInput{
    Key: "examples/image.png", Body: file, ContentType: "image/png",
})
if err != nil {
    return err
}
object, err := store.Get(ctx, "examples/image.png")
if err != nil {
    return err
}
defer object.Body.Close()
_, err = io.Copy(destination, object.Body)
```

切换 OSS 时，其他调用保持一致，连接配置改为：

```go
options.Provider = objectstore.ProviderOSS
options.Endpoint = "https://oss-cn-hangzhou.aliyuncs.com"
options.Region = "cn-hangzhou"
options.Bucket = "production-files"
options.UsePathStyle = false
options.AllowInsecureHTTP = false
// 使用目标 bucket 的 RAM 凭证或 STS 临时凭证。
options.AccessKeyID = accessKeyID
options.AccessKeySecret = accessKeySecret
options.SessionToken = sessionToken // 非 STS 时留空
```

切换配置不迁移已有数据。需先把对象复制到目标 bucket，保留对象 key 和元信息，完成数据核对后切换连接。数据库应保存对象 key 等定位信息；临时签名 URL 会过期，不应作为永久文件地址。bucket 命名、文件归属、上传授权、保留与删除规则由应用管理，框架不包含业务接口。

配置使用 `mapstructure` 标签，可组合到应用配置结构。应用需将组件默认值映射到配置加载器的 `Defaults`，不能依赖提前给目标结构赋值；加载后调用 `Validate`。真实凭证由运行环境提供，SDK 不自行查找云环境凭证。首版使用固定凭证，可选 `SessionToken`；临时凭证不会自动刷新，应用须在到期前更新实例。

`Put` 接受可 Seek 的正文（如文件或 `bytes.Reader`），从当前游标上传剩余部分，校验时临时 Seek 到末尾并恢复；调用方负责关闭和独占使用正文。单次 PUT 最多 5 GiB，不将整个文件读入内存，不自动重试或启动分片上传。SDK 的请求重试固定为一次尝试；标准 HTTP Transport 对可重放请求仍可能有有限连接恢复行为，超时或断连不能证明写入未执行。服务端覆盖同名对象的默认行为保留，版本控制和保留策略由 bucket 配置决定。

`Get` 正文必须读取至 EOF 或关闭，`Stat` 返回大小、类型、ETag、修改时间及元信息。对象 key 不做路径清理或业务改写；公共范围是有效 UTF-8、1–1024 字节，不以 `/` 或 `\` 开头。bucket 使用 3–63 位小写字母/数字/连字符且两端为字母数字，不支持带点的 bucket。元信息名称为小写 ASCII 字母、数字或连字符，总键值长度最多 2048 字节。不把 ETag 视为 MD5，不承诺不同提供方使用相同 ETag 或校验算法；保留 OSS SDK 的上传 CRC64 校验，S3 的可选自动校验设为 when-required 以兼容本地服务。当前不提供跨提供方内容摘要验证。

文件下载保持存储的原始字节（包含已存储的 gzip 数据），不会通过 HTTP 透明解压改写。`OperationTimeout` 默认 5 分钟，覆盖 SDK 操作及下载正文消费；调用方 Context 可以缩短它。大文件可按所需传输预算调整，底层 `HTTP.RequestTimeout` 也是每次网络交换的总期限，实际取较短者。HTTP 配置默认不限制响应正文大小，强制禁用重定向；TLS 校验、连接池、代理和中间件复用 `httpclient`。`New(options, logger, middleware...)` 可注入追踪等标准 RoundTripper 中间件，遵守原 HTTP 中间件的并发与资源契约。

`PresignGet(ctx, key, expires)` / `PresignPut(...)` 返回 URL、Method、Headers 和到期时间，期限要求 1 秒至 7 天的整秒。签名仅在本地生成，不证明对象存在或凭证有效；实际有效期还受 STS 凭证、服务端策略限制。调用方须原样使用 URL 和必需请求头；签名 PUT 不包含业务文件类型/大小限制。`PublicEndpoint` 可为链接指定外部可访问的服务地址，默认使用 Endpoint，仍遵守相同 bucket 寻址规则；不支持通过简单替换 URL 改成 CDN、任意 CNAME 或应用代理地址。新环境的域名、证书、bucket CORS 与访问权限需分别配置。

日志仅记录操作、提供方、耗时及安全错误分类；底层 HTTP 诊断不记录对象路径、查询、凭证、正文或元信息。`Options`、`Client`、`SignedRequest` 的常见格式化会隐藏凭证或链接；签名结果 JSON 有意包含链接以交付客户端，不能整体写日志。`SafeError` 保留 `errors.Is/As`，可识别 `ErrNotFound`、`ErrBucketNotFound`、`ErrAuthentication`、`ErrPermission`、`ErrUnavailable`、`ErrClosed` 及 Context 错误。HEAD 返回的无明确错误码 404 不解释为 `ErrNotFound`，因为它可能来自 bucket 或权限问题；不要记录解包后的 SDK 原始错误。

`Ping` 使用 S3 HeadBucket / OSS GetBucketInfo，需要相应 bucket 权限，不代表对象读写权限全部有效。组件不自动成为启动/就绪依赖。`Shutdown(ctx)` 拒绝新操作，等待 SDK 调用和下载流结束，上限为调用方期限与 `HTTP.ShutdownTimeout` 的较短者；超时会取消在途操作。`Close` 立即取消并有界等待。先停止请求入口和后台任务，再关闭存储客户端，最后关闭日志；不能在尚未结束的下载消费中等待自身排空。不响应取消的调用方 Reader/Seek 无法强行终止，关闭超时不代表所有调用均已退出。

独立测试运行真实 SDK 和本地 HTTP 服务，包含双提供方协议、签名、流式正文、取消及并发关闭：

```powershell
go test -count=1 -race ./objectstore
```

真实集成测试需要专用 bucket。通过 `OBJECTSTORE_TEST_ENDPOINT`、`OBJECTSTORE_TEST_PROVIDER`（s3/oss）、`OBJECTSTORE_TEST_REGION`、`OBJECTSTORE_TEST_BUCKET`、`OBJECTSTORE_TEST_ACCESS_KEY_ID`、`OBJECTSTORE_TEST_ACCESS_KEY_SECRET` 提供配置；可选 `OBJECTSTORE_TEST_SESSION_TOKEN`、`OBJECTSTORE_TEST_PUBLIC_ENDPOINT`、`OBJECTSTORE_TEST_TLS_ROOT_CERT`。HTTP 开发服务须显式设置 `OBJECTSTORE_TEST_ALLOW_HTTP=true`；S3 默认 path-style，OSS 默认 virtual-host，可用 `OBJECTSTORE_TEST_PATH_STYLE` 显式配置。测试只清理本次 `framework-test/<随机值>/` 下的两个对象，不删除 bucket 或清理其他对象；未提供 Endpoint 时明确跳过。

```powershell
go test -count=1 -run '^TestIntegrationObject' -v ./objectstore
```

参考：[OSS S3 兼容差异](https://help.aliyun.com/zh/oss/developer-reference/compatibility-with-amazon-s3)、[OSS Go SDK v2](https://github.com/aliyun/alibabacloud-oss-go-sdk-v2)、[S3 Go SDK 端点配置](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-endpoints.html)。

## 出站 HTTP 客户端

`httpclient` 基于标准库 `net/http`，提供独立 Transport、连接复用、HTTP/1.1 与 HTTPS HTTP/2、认证代理、TLS 校验、按次期限、响应限制、安全日志及关闭能力。它不定义任何业务协议，也不依赖需求草稿中的具体模块。

```go
options := httpclient.DefaultOptions()
client, err := httpclient.New(options, logger)
if err != nil {
    return err
}
defer client.Close() // 资源兜底；正常退出使用 Shutdown

req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/resource", nil)
if err != nil {
    return err
}
resp, err := client.Do(req)
if err != nil {
    return err
}
defer resp.Body.Close()
// 4xx/5xx 是正常取得的 HTTP 响应，状态处理由调用方决定。
if resp.StatusCode != http.StatusOK {
    return fmt.Errorf("unexpected HTTP status: %d", resp.StatusCode)
}
data, err := io.ReadAll(resp.Body)
// 按需处理 data 与 err；不要把敏感响应正文直接写入日志。
```

默认请求总期限为 30 秒，包含连接等待、上传、重定向及响应体读取；拨号/TLS 握手各 5 秒，响应头等待 10 秒。响应头等待从请求体写完后开始，不限制后续流式读取。调用方更短的 Context 始终有效；响应头超时等标准库错误也可能满足 `errors.Is(err, context.DeadlineExceeded)`，并可通过 `errors.As` 获取 `net.Error`。

默认最多保留 100 条空闲连接，每主机 10 条空闲、20 条总连接。`MaxConnsPerHost` 限制连接数，不是 HTTP/2 并发流数量或业务额度。需要隔离长流和普通请求时，分别构造客户端。默认响应头上限 1 MiB、响应体上限 8 MiB；响应体限制作用于调用方实际读取的字节（包括自动解压后的内容），未知长度的响应最多额外探测一个字节，不整包缓存。

按次配置用 `DoWithOptions(req, httpclient.RequestOptions{...})`；零值字段继承默认值，正的 `Timeout` 覆盖本次总期限，`MaxResponseBytes` 正值覆盖字节上限，`-1` 显式取消字节上限。以下示例直接将流复制到已有的 `writer`，不解析特定协议：

```go
resp, err := client.DoWithOptions(req, httpclient.RequestOptions{
    Timeout: 10 * time.Minute,
    MaxResponseBytes: -1,
})
if err != nil {
    return err
}
defer resp.Body.Close()
_, err = io.Copy(writer, resp.Body)
```

响应体在 `Do` 返回后仍保持可读，直至 EOF、显式关闭、请求取消或期限到达。读取至 EOF 自动释放本组件的请求登记，重复读取仍返回标准 `io.EOF`；调用方仍应 `defer resp.Body.Close()`。流式响应没有额外的“每次 Read 空闲超时”，仍受所选总期限约束。WebSocket、101 协议升级及双向隧道不在本组件支持范围，101 响应返回 `ErrProtocolUpgrade`。

默认不跟随重定向，直接返回 3xx 响应。`MaxRedirects > 0` 时最多跟随指定次数，仅允许同源（协议、主机、有效端口与 URL 用户信息一致），拒绝 HTTPS 降级并移除自动 Referer。重定向的请求方法/正文重放遵循标准库规则。不提供额外的请求或状态码重试；标准 Transport 对已使用连接上可重放请求的有限网络恢复仍然有效，不能宣称所有请求都只发送一次。外部操作是否完成、是否允许重试由接入方核实。

TLS 至少使用 TLS 1.2，始终验证证书与主机名；`TLSRootCert` 指定该客户端的信任 PEM，不修改全局证书。默认不读取环境代理；设置 `UseEnvironmentProxy` 后使用标准库环境代理规则，或通过 `ProxyURL` 配置 HTTP(S) 代理，二者互斥。代理 URL 可以带认证信息，格式化和 JSON 不输出该 URL；真实凭证由应用运行环境提供。组件不会自动转发请求 ID 头或添加业务认证头。

慢/失败日志记录标准方法、协议（`scheme: http/https`）、目标主机、状态、总耗时、响应头耗时、已读取字节及是否完整读取，并复用 Context 的请求 ID。未知方法显示 `OTHER`，非标准主机诊断使用固定占位；URL 路径、查询、片段、用户信息、请求/响应头和正文不进入日志。`SlowThreshold` 的总耗时包含调用方消费响应的时间，长流可用独立客户端调整或设为零关闭慢日志；非 2xx 中的 4xx/5xx 在响应收尾时单独记录状态诊断。

直接返回的错误经过安全诊断边界；`errors.Is` 可识别标准 Context 错误以及 `ErrClosed`、`ErrResponseTooLarge`、`ErrBodyClosed`、`ErrRedirectLimit`、`ErrRedirectOrigin`、`ErrRedirectDowngrade`。原始原因仍可通过 `errors.As` 查询，解包后的 URL/网络错误可能含敏感信息，不应直接格式化到日志。

`Shutdown(ctx)` 停止新请求准入，等待已准入请求的响应体读完/关闭及组件日志收尾，再释放空闲连接；上限取调用方期限与 `ShutdownTimeout`（默认 5 秒）的较短者。取消或超时后强制取消在途请求并关闭响应体，返回相应 Context 错误。`Close()` 立即取消后限时等待收尾；两者可重复、并发调用，任一强制关闭都会终止全局排空。应用先停止请求入口和任务，再关闭客户端，最后关闭日志；不要在尚未结束的响应消费流程内同步等待自身排空。

`Client` 实现标准 `Do(*http.Request) (*http.Response, error)`，可供支持该接口的 SDK 注入；不提供原始 `http.Client` 或 Transport 的获取/替换方法。`CloseIdleConnections()` 只回收空闲连接，不停止准入。交给 `Do` 后，请求体遵循标准客户端的关闭所有权；上传读取器应支持 `Read` 与 `Close` 并发，并在关闭后退出。请求对象不得在使用中被并发修改。

构造时可追加 `Middleware`，类型为 `func(http.RoundTripper) http.RoundTripper`；已有 `New(options, logger)` 用法保持可用。`New(options, logger, A, B)` 按 `A → B → Transport` 发出请求，响应反向返回；每个客户端在构造时固定组装一次，重定向的每一跳均经过这条链。`RoundTripperFunc` 可将函数适配成标准接口，例如：

```go
injectHeader := func(next http.RoundTripper) http.RoundTripper {
    return httpclient.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
        clone := req.Clone(req.Context())
        clone.Header.Set("X-Example", "value")
        return next.RoundTrip(clone)
    })
}
client, err := httpclient.New(options, logger, injectHeader)
```

中间件适合 Trace Context 注入及请求交换级指标。`RoundTrip` 返回时通常只收到响应头；原有请求总期限、响应体限制、完整消费耗时和排空跟踪继续在客户端层生效。中间件必须并发安全，修改请求前先 Clone，保留 Context/请求体所有权并正确转交响应体；需要包装响应体时也须转交 Close。不得修改传入 `next` 的连接配置或另建连接池；底层 Transport 由客户端回收，独立的采集器、导出器等资源由调用方管理。nil 中间件及返回 nil（含类型化 nil）的中间件在构造时拒绝。

测试使用真实的本地 HTTP、TLS、HTTP/2 和代理服务器，无需外部服务：

```powershell
go test -count=1 -race ./httpclient
```

## Redis

`redis` 包基于 go-redis/v9 提供单节点连接、认证/TLS、连接池、诊断和限时排空。配置由应用组合，框架不读取业务环境变量，也不把 Redis 自动加入应用启动依赖。

```go
options := redis.DefaultOptions()
options.Host = "127.0.0.1"
options.TLS = false // 仅用于明确配置的本地开发连接；默认启用 TLS 验证
client, err := redis.Open(ctx, options, logger)
if err != nil {
    return err
}
defer client.Close()

err = client.WithClient(ctx, func(work context.Context, commands goredis.Cmdable) error {
    return commands.Set(work, "example:key", "value", time.Minute).Err()
})
```

上例中 `redis` 为本框架包，`goredis` 为 `github.com/redis/go-redis/v9`。`defer Close()` 用于资源兜底；正常退出应先停止请求入口和后台任务，调用 `Shutdown(ctx)`，检查关闭错误并在回调收尾后关闭日志。回调使用提供的 Context，返回命令或 Pipeline Exec 的错误；不得把客户端、Pipeline 或连接对象带出回调。框架支持同步命令及普通/事务 Pipeline，长连接订阅与 Watch 暂不通过此接口暴露。

普通命令与 Pipeline 共用有上限的池；自动命令重试固定关闭，断连后的下一次操作可以重新建立连接。`CommandTimeout` 是 `WithClient` 整个回调的总期限，包含池等待、握手及多个命令，不是每条命令分别计时；回调不提供跨命令原子性。

`WithClientTimeout(ctx, timeout, fn)` 为本次回调指定正的总期限，可以长于默认值，但不会延长调用方 Context 的期限，也不放宽普通命令的读写超时。`BLPop` 等阻塞命令由 go-redis 按命令设置读取期限，仍受回调总期限约束。长期阻塞消费者宜使用独立客户端/连接池，避免占用普通请求的池。例如服务端等待 30 秒时，可给回调留出额外连接和收尾预算：

```go
err = client.WithClientTimeout(ctx, 35*time.Second, func(work context.Context, commands goredis.Cmdable) error {
    return commands.BLPop(work, 30*time.Second, "example:queue").Err()
})
```

`Shutdown(ctx)` 先拒绝新回调和 `Ping`，保持已准入回调的 Context 与连接可用，等待它们完成后释放连接。排空及资源回收的总等待上限取调用方期限与 `ShutdownTimeout` 的较短者；取消或超时后强制关闭并返回对应 Context 错误。`Close()` 立即取消、关闭连接，然后在 `ShutdownTimeout` 内等待回调收尾。两者都支持重复和并发调用；任一 `Close()` 或 `Shutdown` 的取消/超时都会终止全局排空。关闭方法须在回调外调用。

已开始的网络 I/O 遵守设置好的期限，提前取消后可能等到 I/O 期限才退出；强制关闭会主动关闭套接字。取消、超时、断连都不能证明写命令未执行，应用应核实原操作结果。不响应取消的用户回调可能在关闭方法返回后继续运行，超时错误不代表全部回调已经结束。

`errors.Is(err, redis.Nil)` 判断不存在，`ErrAuthentication`、`ErrPermission`、`ErrPoolTimeout`、`ErrClosed` 及标准 Context 错误用于分类；错误链仍支持 `errors.As`。慢/失败日志记录耗时、命令数量和标准命令名（如 `GET`、`HGETALL`），多命令 Pipeline 聚合最多八种命令及各自数量，超出部分仅记录数量。未知模块/自定义命令名显示为 `OTHER`。日志和直接返回的错误不展开键、值、密码、子命令参数或脚本正文，调用方也不要打印原始命令、参数或手动解包后的驱动错误。`Ping` 可加入应用的就绪检查，`Stats` 提供池统计快照。

真实 Redis 集成测试使用 `REDIS_TEST_HOST`、`REDIS_TEST_PORT`、`REDIS_TEST_USER`、`REDIS_TEST_PASSWORD`、`REDIS_TEST_DATABASE`、`REDIS_TEST_TLS`，TLS 自签 CA 可由 `REDIS_TEST_TLS_ROOT_CERT` 指定。数据库编号与 TLS 开关必须显式设置；未提供 Host 时跳过。使用专用测试账号，允许 `framework-test:*` 随机测试键的普通命令、Pipeline、Lua 和阻塞列表操作；只清理本测试自身的键，不执行清库命令。

```powershell
go test -count=1 -run '^TestIntegration' -v ./redis
```

## 模块

需要 Go 1.27.2 或更高版本，模块路径为：

```text
github.com/cauliflower-beep/easy-go-framework
```

仓库托管在 GitHub，其他项目可直接按版本引用，无需配置内网 Git 地址映射：

```powershell
go get github.com/cauliflower-beep/easy-go-framework@v0.1.0
```

按需导入组件，例如 `github.com/cauliflower-beep/easy-go-framework/config` 或 `github.com/cauliflower-beep/easy-go-framework/httpserver`。开发环境需要满足上述 Go 工具链要求；完整接入方式见本文开头的示例应用。

## 检查

在仓库根目录运行 PowerShell 脚本，执行格式检查、静态检查、测试和构建。脚本兼容 Windows PowerShell 5.1 与 PowerShell 7，含中文的 `.ps1` 使用 UTF-8 with BOM 保存；仓库 `.editorconfig` 固定该编码：

```powershell
.\scripts\check.ps1
```

使用 `-Race` 加入并发检测；检查脚本禁用父工作区，并以 `-count=1` 执行测试，避免缓存掩盖当前状态：

```powershell
.\scripts\check.ps1 -Race
```

普通检查中真实依赖测试在缺少连接配置时明确跳过；通过不等于验收真实服务或 Linux 运行。显式验收使用以下入口，缺少所选组件必需的环境变量会直接失败，不会以跳过代替通过：

```powershell
.\scripts\check-integration.ps1 -Redis -Taskqueue -Race
.\scripts\check-integration.ps1 -Postgres -Objectstore
```

Redis 使用本页列出的 `REDIS_TEST_*`；Taskqueue 使用独立的 `TASKQUEUE_TEST_*`，脚本仅在显式选择 `-Taskqueue` 时临时启用其测试开关。PostgreSQL 需要 `POSTGRES_TEST_HOST`、`POSTGRES_TEST_USER`、`POSTGRES_TEST_DATABASE`、`POSTGRES_TEST_SSL_MODE`；可用 `POSTGRES_TEST_PORT`/`POSTGRES_TEST_PASSWORD` 补充连接参数。对象存储使用上文的 `OBJECTSTORE_TEST_*`。使用专用资源和最小所需权限；脚本不自动部署外部服务、设置持久化或创建云资源。Linux 交付还须在目标系统执行检查及真实依赖测试，交叉构建不能替代运行验收。
