# telegram-antispam-bot

使用 Go、`github.com/go-telegram/bot` 和 Jev/System One 兼容决策接口的 Telegram 群反垃圾机器人。
识别重点是诈骗、黑灰产推广；默认启动为 **只记录模式**。

## Docker / Compose 启动

适用于 Linux 服务器，支持 Docker Compose 和 Podman Compose。
镜像默认使用 UID/GID `65532:65532`；Compose 按 `.env` 中的 UID/GID 运行，以便与宿主机文件权限一致。

### 1. 构建镜像

```sh
docker build -t telegram-antispam-bot:local .
```

构建阶段使用 Go 1.26；运行镜像包含静态程序和 HTTPS 根证书。
构建上下文仅包含 Go 源码与依赖清单，本地配置、密钥和数据库保留在宿主机。

### 2. 创建配置和环境变量文件

```sh
mkdir -p runtime
chmod 700 runtime
(set -C; umask 077; docker run --rm --pull=never telegram-antispam-bot:local -example > runtime/config.json)
cp -n .env.example .env
chmod 600 .env
id -u
id -g
```

编辑 `.env`：

- `BOT_UID`、`BOT_GID`：填上面两个命令的输出；宿主机 root 用户通常均为 `0`。
- `TELEGRAM_BOT_TOKEN`：填 BotFather 提供的令牌。
- `SYSTEMONE_API_KEY`：填模型服务密钥；服务关闭鉴权时留空。

rootless Podman 的默认用户命名空间把容器 UID/GID `0:0` 映射到运行 Podman 的宿主机用户，此时可使用 `BOT_UID=0`、`BOT_GID=0`。

编辑 `runtime/config.json`，至少设置 `owner_ids`、`model.base_url` 和 `model.name`。
下面是一份可独立使用的精简配置，其余参数使用内建默认值：

```json
{
  "owner_ids": [123456789],
  "model": {
    "base_url": "http://127.0.0.1:8000",
    "name": "replace-with-served-model-name"
  },
  "base": {
    "mode": "record",
    "debug": true,
    "queue_limit": 100
  },
  "groups": {
    "-1001234567890": {
      "rules": {
        "repeat": { "duration_seconds": 120 }
      }
    }
  }
}
```

将数字 ID 和模型名替换成实际值。`groups` 也可以设为 `{}`，让所有群使用共享配置。
`base.mode=record` 是只记录模式；`base.debug=true` 把各题概率写入日志。

Compose 使用宿主机网络，本机模型服务可以填写 `http://127.0.0.1:8000`。
远程服务填写它的实际地址，程序追加 `/v1/systemone`。
Docker Desktop 4.34 及更新版本需要先启用设置中的 host networking。

### 3. 校验并启动

```sh
docker compose config --quiet
docker compose run --rm -T --no-deps antispam -config /runtime/config.json -check
docker compose up -d
docker compose logs -f antispam
```

把机器人设为群管理员，授予删除消息和限制成员权限。
服务通过 Telegram 主动轮询接收消息。
Podman 环境也可把以上 `docker` 换成 `podman`。

### 4. 管理配置与持久化数据

- `runtime/config.json`：配置文件，Telegram 管理命令会把修改写回这里。
- `runtime/data/bot.sqlite`：默认数据库，保存消息、判定结果、队列和人工标注。
- `runtime/data/`：同时保留 SQLite 的 WAL、共享内存及锁文件。
- `.env`：容器环境变量，保存令牌和运行用户设置。

整个 `runtime` 目录以可写方式挂载，支持配置文件原子替换和数据库写入。
更换镜像、重建容器后，宿主机目录中的数据继续保留。
手工修改 JSON 后，在 Telegram 执行 `/as_reload`；修改 `.env` 后重建容器：

```sh
docker compose up -d --force-recreate
```

更新代码后的重建命令：

```sh
docker compose build
docker compose up -d --force-recreate
```

## 直接运行二进制

需要 Go 1.26，以及 Linux、macOS 或 BSD。SQLite 驱动为纯 Go，数据库随程序运行创建。

```sh
go build -o antispam ./cmd/antispam
# 创建配置；已有同名文件时停止，避免覆盖。
(set -C; umask 077; ./antispam -example > config.json)
```

修改生成的配置：

- `owner_ids`：机器人所有者的 Telegram **数字用户 ID**。
- `model.base_url`：决策服务地址，例如 `http://127.0.0.1:8000`，程序追加 `/v1/systemone`。
- `model.name`：服务实际提供的模型名，可以通过该服务的 `GET /v1/models` 查询。
- `groups`：把示例群 ID 换成实际 ID，或者先使用空对象 `{}`，让所有群继承共享配置。

```sh
./antispam -config config.json -check
export TELEGRAM_BOT_TOKEN='替换成机器人令牌'
export SYSTEMONE_API_KEY='替换成决策服务密钥'  # 本地服务关闭鉴权时可省略
./antispam -config config.json
```

把机器人添加为群管理员，授予读取群消息、删除消息和限制成员的权限。
推荐使用超级群；限时禁言依赖 Telegram 的超级群限制成员接口。
程序使用 long polling；已有 webhook 时启动会提示先移除 webhook。

`-check` 仅校验配置；`-example` 向标准输出打印完整示例。
数据库路径相对于配置文件所在目录解析，默认是 `data/bot.sqlite`。
同一数据库由一个机器人进程独占；重启时会恢复已收到的更新和剩余待判任务。

## 配置与处罚规则

`base` 是共享配置；`groups` 保存群级差异。覆盖逐字段合并：

```json
{
  "base": {
    "mode": "record",
    "debug": false,
    "queue_limit": 100,
    "rules": {
      "repeat": {
        "enabled": true,
        "question": "violation",
        "threshold": 0.85,
        "count": 3,
        "window_seconds": 600,
        "action": "mute",
        "duration_seconds": 600
      }
    }
  },
  "groups": {
    "-1001234567890": {
      "rules": {
        "repeat": { "duration_seconds": 120 }
      }
    }
  }
}
```

这是完整配置中的策略部分。示例群最终使用“600 秒内至少 3 条消息命中，禁言 120 秒”，其余参数继承共享值。
`false` 和 `0` 按明确设置处理；删除覆盖项即可恢复继承。配置拒绝未知字段、重复键和 `null`。
文件中的 `base.rules` 和顶层 `questions` 一旦显式提供，就定义完整集合；省略时使用内建默认值。
例如 `/as_set base rules {}` 清空共享规则；群级 `rules` 继续逐规则、逐字段覆盖。

| 参数 | 含义 |
| --- | --- |
| `mode` | `record` 保存判定及拟采取的动作；`enforce` 开启自动处罚 |
| `debug` | 在服务器 JSON 日志中输出各题概率 |
| `queue_limit` | 本群最多保留的待判版本数，包括正在调用模型的版本；范围 1–100000 |
| `rules.<名称>.enabled` | 启用或关闭这条规则 |
| `question` / `threshold` | 使用哪道题的“是”概率；达到或超过阈值即命中 |
| `count` / `window_seconds` | 同一群、同一发言者、同一规则下的去重触发次数和窗口；单条规则可设为 `1` / `0` |
| `action` | `delete`、`mute` 或 `kick` |
| `duration_seconds` | 禁言秒数，范围 60–31536000；删除和踢人填 `0` |

支持增加规则、修改题目、按不同题目的概率配置不同动作。
同时命中多条规则时可以删除消息并处罚成员；成员处罚优先踢人，其次取最长禁言时长。
踢人后成员可以通过有效邀请重新加入。

默认配置含单条删除规则和累计禁言规则；概率值是内测起点，先用真实群消息和人工标注评估再开启处罚。
机器人自身、机器人所有者和群管理员保留自动处罚豁免，相关消息及判定结果照常留档。
频道身份发言支持删除消息，成员禁言和踢人记录为不适用。

### 问题组、自定义加减分与最终条件

`examples/scoring.json` 是可直接校验的完整配置：包含 6 组共 24 道候选题，共享策略选用其中 9 道，两个群展示局部覆盖。复制后设置自己的所有者 ID、模型名和群 ID：

```sh
cp -n examples/scoring.json config.json
chmod 600 config.json
./antispam -config config.json -check
```

容器部署时将目标路径换成 `runtime/config.json`。示例保持 `mode: "record"`；权重和阈值用于试运行，使用本群样本评估后再开启处罚。

- `question_groups.<组名>.<题名>` 定义题目，包含 `type`、`instructions` 和可选的正反标准或选项 `criteria`。
- `base.questions` / `groups.<群ID>.questions` 选择单题，例如 `"message.promotion"`，或整组，例如 `"profile.*"`。数组整体替换；重复选择、空数组和未知题目在重载时拒绝。每群最多选 256 题。
- 省略选题时继续使用原来的顶层 `questions`。旧配置、`-example` 生成的五题配置和单题概率规则保持兼容。
- `scoring.initial` 是初始分；`scoring.rules` 是整组替换的加减分项数组，每项包含唯一的 `name`、布尔表达式 `when`、有符号数值 `add`。匹配项全部累计，负数扣分，最终总分允许为负数。
- `condition` 是最终门槛。仅成立时进入已有的累计次数与动作处理；省略该字段保留原有规则行为。

以下为完整配置里的策略片段；题库中需要定义并选中表达式引用的各题：

```json
{
  "questions": ["profile.promotion", "message.promotion", "benign.warning", "decision.fraud"],
  "scoring": {
    "initial": 0,
    "rules": [
      {"name": "资料推广", "when": "p.profile.promotion >= 0.85", "add": 20},
      {"name": "消息推广", "when": "p.message.promotion > 0.80", "add": 50},
      {"name": "风险提醒", "when": "p.benign.warning >= 0.90", "add": -50}
    ]
  },
  "condition": "score >= 60 || p.decision.fraud >= 0.98",
  "rules": {
    "delete": {"enabled": true, "count": 1, "action": "delete"},
    "repeat": {"enabled": true, "count": 3, "window_seconds": 600, "action": "mute", "duration_seconds": 600}
  }
}
```

这里的处置规则省略 `question` 和 `threshold`，以最终 `condition` 成立作为一次命中。继续保留 `question` 的旧规则还会检查该题概率门槛。每条消息在同一规则、同一计分定义下计一次；更改题目定义、计分或最终条件会启动独立的累计系列。处置次数、窗口和禁言时长的调整继续使用同一系列。

表达式支持 `>`、`>=`、`<`、`<=`、`==`、`!=`、`&&`、`||`、`!` 和括号：

| 引用 | 含义 |
| --- | --- |
| `p.message.fraud` | 是非题 `message.fraud` 的“是”概率 |
| `p.context.kind.spam` | 选择题 `context.kind` 中 `spam` 选项的概率 |
| `s.message.severity` | 评分题 `message.severity` 返回的期望等级，等级从 0 起算 |
| `score` | bot 计算的总策略分，仅用于最终 `condition` |

所有概率范围为 `[0,1]`。组名、题名和选择题选项名使用英文字母开头、最多 64 位的字母数字下划线标识符。每项 `when` 独立读取答案；计分顺序保持无关。表达式按类型和已选题目预先校验，函数调用、任意代码和算术表达式会被拒绝。每个表达式最多 4096 字节，每份策略最多 128 个计分项。

### 置信度与逐题弃答

共享和群级策略支持：

```json
{
  "min_confidence": 0.75,
  "on_low_confidence": "abstain_question"
}
```

**是非题 `noul` 直接使用概率阈值**：例如 `p.message.fraud > 0.9`，返回有效概率即可参与计分，`confidence` 对它的采用状态没有影响。

**选择题 `choice` 和评分题 `score` 使用服务返回的 `confidence`**：低于阈值或缺少该字段时，将这道题记为 `unknown`。等于阈值时采用。默认阈值为 `0`，此时服务仍需提供有效的置信度。bot 只使用服务原值，概率和置信度分别留档。

两种题型的定义示例，可放入某个问题组：

```json
{
  "kind": {
    "type": "choice",
    "instructions": "当前作者的主要发言用途是什么？",
    "criteria": {"spam": "主动广告或引流", "ham": "正常讨论或风险提醒", "other": "其他用途"}
  },
  "severity": {
    "type": "score",
    "instructions": "当前消息呈现的诈骗诱导程度有多高？",
    "criteria": ["正常交流", "存在可疑招揽", "明确诱导交出凭据或转移资产"]
  }
}
```

弃答属于一次成功的推理结果，队列完成处理并保留原始答案。引用弃答题的计分项整项跳过；即使该项的 `when` 存在其他成立的 OR 分支，仍整项跳过。其余加减分项照常累计。

最终 `condition` 使用三值逻辑：`true`、`false`、`unknown`。`true || unknown` 为 `true`，`false && unknown` 为 `false`，其余依赖未知答案的结果保留 `unknown`；对 `unknown` 取反仍为 `unknown`。只有 `true` 触发动作。所有分数、逐项命中或跳过原因、最终条件结果均保存到 `evaluations` 表。

题库按实际材料选用：当前输入包含可获取的资料、完整消息、回复与引用；`crosslink.repeated_promotion` 需要额外提供本群近期记录，因此示例共享策略暂时排除该题。涉及个人频道、可信身份或群规的题目也应按已提供的材料与群内场景启用或调整。题目数量与识别效果需要通过实际样本验证。

## Telegram 管理命令

群内命令可加上 `@机器人用户名` 后缀。普通群成员的命令形状消息继续参与检测。

| 命令 | 权限与作用 |
| --- | --- |
| `/as_help` | 显示帮助；也支持私聊 `/start` |
| `/as_config` | 群管理员：查看本群最终配置及本群覆盖项 |
| `/as_set mode "record"` | 群管理员：切换本群模式，启用处罚时改为 `"enforce"` |
| `/as_set queue_limit 50` | 群管理员：修改本群待判容量，缩小时立即淘汰最旧待判版本 |
| `/as_set questions ["message.promotion","decision.fraud"]` | 群管理员：替换本群选题；计分和条件引用需要与新选题一致 |
| `/as_set condition "score >= 75"` | 群管理员：修改最终处置门槛 |
| `/as_set min_confidence 0.85` | 群管理员：修改选择题与评分题的置信度门槛 |
| `/as_set scoring.rules [{"name":"诈骗","when":"p.decision.fraud > 0.9","add":80}]` | 群管理员：替换本群加减分项 |
| `/as_set rules.repeat.duration_seconds 120` | 群管理员：修改本群某条规则的参数 |
| `/as_reset rules.repeat.duration_seconds` | 群管理员：恢复该参数的共享继承 |
| `/as_status` | 群管理员：查看本群版本处理统计 |
| `/as_spam` / `/as_ham` | 群管理员：回复原消息，标注广告或正常消息 |
| `/as_spam 123` / `/as_ham 123` | 群管理员：按本群消息 ID 标注数据库中的消息，适用于原消息已删除的情况 |
| `/as_config base` | 所有者：查看共享配置 |
| `/as_set base debug true` | 所有者：修改共享参数；例如打开概率调试日志 |
| `/as_reload` | 所有者：校验并重载服务器文件 |

**回复原消息并 @本机器人**也会标注为 `spam`；标注权限属于本群管理员。
机器人所有者管理共享配置；执行本群管理命令时也需要本群管理员身份。
匿名管理员需要切换为个人身份执行管理命令。
人工标注与模型结果分别保存；标注操作保留原始证据。

命令修改立即生效并原子写回配置文件，重启后保留。
手工编辑文件后执行 `/as_reload`；文件校验失败时继续使用当前配置。
命令检测到文件已被外部修改时会要求先重载，保护手工编辑的内容。
数据库位置和 Telegram 连接设置的变更在重启后生效；模型连接、题目及策略支持重载。
问题组定义通过服务器配置文件修改后执行 `/as_reload`。选题、计分项和条件需要同时调整时，推荐一次编辑文件并校验重载，保持整份配置有效。

## 消息材料与模型协议

每份判断材料含发言者名字、用户名、可获取的简介，以及完整的 Telegram 消息 JSON。
正文、媒体配文、格式化实体、隐藏链接目标、按钮文字与链接、inline bot 来源、转发、回复和其他 API 字段均保留。
额外展开格式化实体的可见文字，正确处理 Telegram 使用的 UTF-16 偏移量。

简介通过额外查询获取并短期缓存，查询结果附带获取状态；Telegram 返回的资料可用性受其接口限制。
媒体消息保留 API 已提供的文字和元数据；图片、二维码、音视频内容识别属于后续范围。
Telegram Bot API 对其他机器人直接发送的群消息存在接收限制，用户经由 inline bot 发布的消息属于检测范围。

默认问题是：推广意图、诈骗诱导、黑灰产招揽、正常讨论语境、最终违规判断。
顶层 `questions` 或 `question_groups` 可在配置中修改或扩展，支持 `noul`、`choice`、`score`，每题包含 `instructions`。`noul` 的 `criteria` 可省略；提供时包含 `true`、`false` 两项说明。`choice` 包含 2–255 个选项；`score` 包含 2–10 个从低到高的等级说明。
客户端发送 `state`、`model`、已选 `questions`，保留题目标准，验证模型名、答案类型、完整性、概率分布和等级范围。协议错误仍进入失败重试；合法答案的低置信度按逐题弃答处理。
所有问题共同使用这条消息的材料，各题独立作答。

每次编辑创建新版本并重新排队。队列中保留的版本逐一判断；旧版本结果用于留档，最新版本负责触发统计和处罚。
累计统计中同一条消息对同一规则计一次；窗口基于接收时间。只记录模式与执行模式的累计历史分别计算。
改变题目或概率门槛会开启独立的累计系列。

## 异常、恢复与留档

模型超时、不可用或返回异常时，原消息与失败记录入库，待判任务按 `model.retry_seconds` 的间隔重试。
每群队列满时，新版本挤掉最旧的待判版本；淘汰状态为 `evicted`，原始记录继续保留。
模型调用中的版本也可能被淘汰，它随后返回的结果保留为证据。
模型恢复后继续处理剩余任务。

每个消息版本保存接收时的题目和策略快照，首次查询到的资料随模型输入保存。
自动处罚发送前再次核对当前模式、规则、题目、最新消息版本和目标身份。
记录模式接收的消息保持观察性质；已实际发送的处罚对同一消息和同种动作进行去重。
处罚过程中重启导致远端结果未确认时，动作记为 `uncertain`，供管理员查看。

SQLite 中的主要表：

| 表 | 记录内容 |
| --- | --- |
| `updates` | Telegram 原始更新，接收后先持久保存再推进轮询位置 |
| `revisions` | 每个消息版本、策略快照、模型输入和队列状态 |
| `attempts` | 每次模型请求、完整响应、是非题概率、错误和耗时 |
| `evaluations` | 每次成功推理的原始题目结果、服务置信度、有效/弃答状态、逐项计分、总分和三值条件，按 `attempt_id` 关联 |
| `hits` | 按消息去重的规则触发历史 |
| `actions` | 拟采取的动作、实际执行状态及错误 |
| `annotations` | 人工标注历史及原消息快照 |

日志写入标准错误，使用 JSON 格式。令牌和 API 密钥通过环境变量读取。
数据库从版本 1 自动升级到版本 2，既有消息、模型尝试、队列和动作记录全部保留；历史尝试保持原始记录，新推理开始附带计分审计。
数据库包含群消息和用户资料，文件权限为 `0600`，默认持续保留全部记录。
运行中备份请使用 SQLite 的备份能力；停机复制时一并保留数据库及尚存的 WAL 文件。

## 验证

```sh
go test -race ./...
go vet ./...
go build ./cmd/antispam
```

构建镜像后可运行真实容器集成测试，测试期间使用本地模拟 API：

```sh
ANTISPAM_CONTAINER_IMAGE=telegram-antispam-bot:local go test -race ./tests/container -count=1 -timeout=90s
```

容器测试覆盖非 root 默认用户、HTTPS 根证书、只读根文件系统、命令写回、数据库持久化和重启恢复；同时通过仓库中的 Compose 定义运行同一组流程。

测试使用本地 HTTP 服务和真实 SQLite，覆盖 API 协议、原始字段保存、配置权限、逐版本处理、异常积压、恢复和处罚。
正式启用前在测试群核对机器人权限、资料获取情况及模型实际效果。
