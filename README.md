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

构建阶段使用 Go 1.26；运行镜像基于 Alpine 3.24，包含静态程序、HTTPS 根证书和用于解码动图的 `ffmpeg`，构建时需要访问 Alpine 软件源。Compose 保持根文件系统只读，并在 `/tmp` 挂载 64 MiB tmpfs 供动图抽帧暂存。
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
`base.mode=record` 是只记录模式；`base.debug=true` 开启消息处理、模型结果和计分处置的调试日志。

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

`-check` 仅校验配置；`-example` 向标准输出打印包含 2 组 8 道题和计分规则的完整示例，与 `examples/scoring.json` 共用题库。示例嵌入二进制，任意工作目录和容器内均可生成。
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

这是使用内建五题的传统单题策略片段，用于演示群级继承。示例群最终使用“600 秒内至少 3 条消息命中，禁言 120 秒”，其余参数继承共享值。
`false` 和 `0` 按明确设置处理；删除覆盖项即可恢复继承。配置拒绝未知字段、重复键和 `null`。
文件中的 `base.rules` 一旦显式提供，就定义完整规则集合；省略时使用内建默认规则。题库使用 `question_groups` 或显式的顶层 `questions`；两者均省略时使用内建五题。
例如 `/as_set base rules {}` 清空共享规则；群级 `rules` 继续逐规则、逐字段覆盖。

| 参数 | 含义 |
| --- | --- |
| `mode` | `record` 保存判定及拟采取的动作；`enforce` 开启自动处罚 |
| `debug` | 输出本群的 DEBUG 阶段日志、各题结果、计分与处置依据；支持共享默认和群级覆盖 |
| `queue_limit` | 本群最多保留的待判版本数，包括正在调用模型的版本；范围 1–100000 |
| `rules.<名称>.enabled` | 启用或关闭这条规则 |
| `question` / `threshold` | 使用哪道题的“是”概率；达到或超过阈值即命中 |
| `count` / `window_seconds` | 同一群、同一发言者、同一规则下的去重触发次数和窗口；单条规则可设为 `1` / `0` |
| `action` | `delete`、`mute` 或 `kick` |
| `duration_seconds` | 禁言秒数，范围 60–31536000；删除和踢人填 `0` |

支持增加规则、修改题目、按不同题目的概率配置不同动作。
同时命中多条规则时可以删除消息并处罚成员；成员处罚优先踢人，其次取最长禁言时长。
踢人后成员可以通过有效邀请重新加入。

内建传统配置含单条删除规则和累计禁言规则；当前十七题示例启用单条删除、关闭累计禁言，并保持只记录模式。概率值是试运行起点，先用真实群消息和人工标注评估再开启处罚。
群主、群管理员和匿名管理员的普通发言按 `ignored` 留档，跳过模型判断与自动处罚；合法管理命令继续执行。机器人自身输出同样跳过判断，机器人所有者继续享有自动处罚豁免。
频道自动同步到关联讨论群的消息按 Telegram 的 `is_automatic_forward` 标记跳过。普通成员手动转发频道内容、回复同步帖子的消息继续检测。
管理员身份在接收和模型处理前查询确认；查询异常会记录警告并保留原有检测流程，自动处罚前仍会重新核验身份。
频道身份发言支持删除消息，成员禁言和踢人记录为不适用。

### 全局 bot 黑名单与调用消息清理

黑名单由同一数据库中的所有群共享，按 Telegram 数字 bot ID 匹配，重命名后继续有效。名单命中的 bot 消息完整入库，直接按本群启用的删除规则处理；此路径跳过模型请求和累计次数门槛。管理员、匿名管理员、频道自动同步、当前机器人与所有者的保护继续生效，执行前重新检查权限。

模型判定命中且 bot 消息删除成功后，bot 自动加入名单。所有者可以手动增删查看；`record` 模式保存拟执行删除，自动学习以实际删除成功为依据。手动名单变更在所有群共享，各群的 `record` / `enforce` 模式和删除开关继续独立控制执行。

关联删除使用 Telegram 的 `guest_bot_caller_user` 和同群匹配的回复消息确认调用者。调用消息的原始版本和编辑指纹也要匹配；多个 bot 回复同一调用消息时，调用消息的删除去重，并可采用任一仍有效的已确认来源。尚未派发的记录意图可以在调用消息本身属于执行模式、且出现新的执行证据后升级；原意图与证据切换均留审计。当前作者、被回复者的消息分别完整留档，关联依据保存在独立表中。队列淘汰只结束模型待判工作，已经确认的调用关系仍可用于清理调用消息。

升级首次启动时，数据库从已有 `enforce` 模式的成功 bot 删除记录导入名单。初始化只导入 bot 身份；历史调用消息和历史动作记录保持原样。每次移除记录归档版本边界，重新自动加入采用边界之后的新广告删除证据；旧的待执行名单动作在执行前复核名单代次。

名单管理使用 ID 或本库已观察到的用户名，也可以回复 bot 消息操作。用户名用于定位已记录的身份，实际拦截以 ID 为准。黑名单与关联清理只追加删除动作。
网络成员查询完成后，派发前再次校验当前模式、消息版本及名单证据。名单增删与最终派发边界串行协调，已开始派发的请求完成后再确认对应的名单变更。

### 问题组、自定义加减分与最终条件

`examples/scoring.json` 是可直接校验的完整配置：`content` 组针对 `.message` 中当前消息的文字和图片，`profile` 组针对 `.sender`；两组分别询问宣传、金钱、赚钱承诺、色情灰产业务及二次元性内容。`content.illicit` 补充黑灰产业务招揽判断，`target` 组观察首个有效提及／链接对象的五类资料及调用意图，共 17 道是非题。二次元性内容仅记录，原有两项组合加分和 83%／85% 门槛保持，灰产招揽概率达到 85% 独立加 100 分。问句与正反标准明确指定读取区域，示例群展示待判容量的局部覆盖。复制后设置自己的所有者 ID、模型名和群 ID：

```sh
cp -n examples/scoring.json config.json
chmod 600 config.json
./antispam -config config.json -check
```

容器部署时将目标路径换成 `runtime/config.json`。示例保持 `mode: "record"`；权重和阈值用于试运行，使用本群样本评估后再开启处罚。

- `question_groups.<组名>.<题名>` 定义题目，包含 `type`、`instructions` 和可选的正反标准或选项 `criteria`。
- `base.questions` / `groups.<群ID>.questions` 选择单题，例如 `"content.promotion"`，或整组，例如 `"profile.*"`。数组整体替换；重复选择、空数组和未知题目在重载时拒绝。每群最多选 256 题。
- 分组配置使用 `question_groups` 定义题库，通过 `base.questions` 和群级 `questions` 选题。顶层空 `questions` 字段可以省略。显式提供的旧版平铺题库继续兼容；两种题库均省略时使用内建五题。当前 `-example` 输出分组题库及计分配置。
- `scoring.initial` 是初始分；`scoring.rules` 是整组替换的加减分项数组，每项包含唯一的 `name`、布尔表达式 `when`、有符号数值 `add`。匹配项全部累计，负数扣分，最终总分允许为负数。
- `condition` 是最终门槛。仅成立时进入已有的累计次数与动作处理；省略该字段保留原有规则行为。

以下为完整配置里的策略片段；题库中需要定义并选中表达式引用的各题：

```json
{
  "questions": ["content.*", "profile.*"],
  "scoring": {
    "initial": 0,
    "rules": [
      {"name": "消息宣传金钱或色情", "when": "p.content.promotion >= 0.83 && (p.content.money >= 0.85 || p.content.sexual >= 0.85)", "add": 100},
      {"name": "资料宣传金钱或色情", "when": "p.profile.promotion >= 0.85 && (p.profile.money >= 0.85 || p.profile.sexual >= 0.85)", "add": 100}
    ]
  },
  "condition": "score >= 100",
  "rules": {
    "delete": {"enabled": true, "count": 1, "action": "delete"},
    "repeat": {"enabled": false, "count": 3, "window_seconds": 600, "action": "mute", "duration_seconds": 600}
  }
}
```

这里的处置规则省略 `question` 和 `threshold`，以最终 `condition` 成立作为一次命中。继续保留 `question` 的旧规则还会检查该题概率门槛。每条消息在同一规则、同一计分定义下计一次；更改题目定义、计分或最终条件会启动独立的累计系列。处置次数、窗口和禁言时长的调整继续使用同一系列。

消息区域满足“宣传 ≥ 83%，且金钱或色情 ≥ 85%”时加 100 分；资料区域使用“宣传 ≥ 85%，且金钱或色情 ≥ 85%”，命中也加 100 分。总分达到 100 即满足最终条件，两个区域同时命中时为 200 分。赚钱承诺题保留判断与审计，当前示例的加分条件使用宣传、金钱和色情三项。

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

题库按实际材料选用：当前模型输入提供 `.sender`、`.reply`、`.message` 三个来源区域，原始完整消息单独归档。提及／链接对象的资料归属于对应片段，模型题目分别记录当前发言、发言者资料和目标资料。题目数量、阈值与识别效果需要通过实际样本验证。

## Telegram 管理命令

群内命令可加上 `@机器人用户名` 后缀。普通群成员的命令形状消息继续参与检测。

| 命令 | 权限与作用 |
| --- | --- |
| `/as_help` | 显示帮助；也支持私聊 `/start` |
| `/as_config` | 群管理员：查看本群最终配置及本群覆盖项 |
| `/as_set mode "record"` | 群管理员：切换本群模式，启用处罚时改为 `"enforce"` |
| `/as_set queue_limit 50` | 群管理员：修改本群待判容量，缩小时立即淘汰最旧待判版本 |
| `/as_set questions ["content.*","profile.*"]` | 群管理员：替换本群选题；计分和条件引用需要与新选题一致 |
| `/as_set condition "score >= 100"` | 群管理员：修改最终处置门槛 |
| `/as_set min_confidence 0.85` | 群管理员：修改选择题与评分题的置信度门槛 |
| `/as_set scoring.initial 0` | 群管理员：修改本群初始分 |
| `/as_set rules.repeat.duration_seconds 120` | 群管理员：修改本群某条规则的参数 |
| `/as_set rules.ban.duration_seconds -1` | 群管理员：设置调用者封禁时长，-1 永久；定时使用 60～31536000 秒 |
| `/as_set rules.ban.enabled true` | 群管理员：开启调用者封禁；实际处罚同时要求 enforce 模式 |
| `/as_reset rules.repeat.duration_seconds` | 群管理员：恢复该参数的共享继承 |
| `/as_status` | 群管理员：查看本群版本处理统计 |
| `/as_join on` / `/as_join off` | 群管理员：开启或关闭本群入群检测，保留既有模式与计分配置 |
| `/as_join status` | 群管理员：查看入群检测有效配置和入群任务统计 |
| `/as_join mode record` / `/as_join mode enforce` | 群管理员：入群检测只记录或执行命中移出 |
| `/as_spam` / `/as_ham` | 群管理员：回复原消息，标注广告或正常消息 |
| `/as_spam 123` / `/as_ham 123` | 群管理员：按本群消息 ID 标注数据库中的消息，适用于原消息已删除的情况 |
| `/as_config base` | 所有者：查看共享配置 |
| `/as_set base debug true` | 所有者：修改共享参数；例如打开概率调试日志 |
| `/as_reload` | 所有者：校验并重载服务器文件 |
| `/as_botlist [active\|inactive\|all] [偏移量]` | 所有者：按状态分页查看全局名单，每页 50 个；省略状态默认 active，兼容 `/as_botlist 50` |
| `/as_botban <bot ID 或 @用户名>` | 所有者：加入全局名单；也可回复 bot 消息发送 `/as_botban` |
| `/as_botunban <bot ID 或 @用户名>` | 所有者：移出全局名单；也支持回复 bot 消息 |
| `/as_botget <bot ID 或 @用户名>` | 所有者：查询启用状态、备注、来源、创建和更新时间；也支持回复 bot 消息 |
| `/as_botedit <bot ID 或 @用户名> <JSON>` | 所有者：编辑 note 备注、active 启用状态；回复 bot 时可省略目标 |

**回复原消息并 @本机器人**也会标注为 `spam`；标注权限属于本群管理员。
机器人所有者管理共享配置与全局 bot 黑名单，名单命令也可在私聊使用；执行本群管理命令时也需要本群管理员身份。
匿名管理员需要切换为个人身份执行管理命令。
人工标注与模型结果分别保存；标注操作保留原始证据。

配置命令的修改立即生效并原子写回配置文件，重启后保留。黑名单命令直接写入共享数据库，名单状态与管理审计随数据库持久保存。
手工编辑文件后执行 `/as_reload`；文件校验失败时继续使用当前配置。
命令检测到文件已被外部修改时会要求先重载，保护手工编辑的内容。
数据库位置和 Telegram 连接设置的变更在重启后生效；模型连接、题目及策略支持重载。
问题组定义通过服务器配置文件修改后执行 `/as_reload`。选题、计分项和条件需要同时调整时，推荐一次编辑文件并校验重载，保持整份配置有效。
本次上下文结构升级需要重启新二进制；新进程会同时载入新格式和新配置。后续单纯修改题目、概率门槛或计分可以继续通过 `/as_reload` 生效。
黑名单功能包含数据库升级，使用新二进制重启后生效；所有者可通过 `/as_botlist` 核对初始化名单。

黑名单编辑示例：

```text
/as_botget 123456789
/as_botedit 123456789 {"note":"人工复核：广告资源推广"}
/as_botedit 123456789 {"active":false,"note":"暂停拦截，继续观察"}
/as_botedit 123456789 {"note":""}
/as_botlist inactive
/as_botlist all 50
```

编辑只接受 `note` 字符串和 `active` 布尔值。备注最多 1000 个字符，空字符串清空备注；状态切换沿用加入／移除时的名单代次和派发保护。只改备注会保留拦截状态、名单代次和原始来源。新增仍使用 `/as_botban`，移除采用停用记录，原始消息和名单审计继续保留。

### 广告 bot 调用者封禁

调用者身份来自 Telegram 的 `guest_bot_caller_user` 数字 ID，并与同群回复消息、已归档的人类调用消息及版本指纹核对。广告 bot 消息删除成功后，关联删除调用消息；调用者的封禁由独立的 `rules.ban` 控制。该规则默认关闭：

```json
"ban": {"enabled": false, "target": "caller", "count": 1, "action": "ban", "duration_seconds": -1}
```

在群内按顺序设置时长、开关和执行模式：

```text
/as_set rules.ban.duration_seconds 86400
/as_set rules.ban.enabled true
/as_set mode "enforce"
```

`duration_seconds: -1` 表示永久封禁；正数使用 60～31536000 秒，示例为一天。定时封禁适用于超级群；普通群中的定时封禁记为跳过，关联消息删除继续执行。定时截止点在动作生成时固定，排队后剩余时间不足 30 秒会跳过，避免 Telegram 将其解释为永久。关闭使用 `/as_set rules.ban.enabled false`。已有自定义规则表的配置可先添加上面的完整 `ban` 对象，再使用字段命令修改。

调用消息与广告回复接收时都须带有相同的已启用封禁规则；只有两边均为 `enforce` 且源广告删除成功，才执行关联处罚。`record` 模式保留拟处罚记录。开启开关后，先前按关闭状态接收的调用保持原处置；历史升级只补齐审计关联。

执行前复核当前策略、消息版本、名单证据和群成员权限。群主、管理员、所有者保持保护；已有封禁保持原状态，已离群的调用者仍可封禁。多个广告回复共享同次调用时，调用消息删除和封禁各自去重，并保存每份来源证据。`ban` 使用 Telegram 的 `banChatMember`；现有 `kick` 继续表示允许重新加入的踢出。

Telegram 对超级群／频道封禁会同时清理该用户的群内消息；本地完整原文、模型结果及处置审计持续保留。此功能包含数据库版本 5 升级，需要使用新二进制重启。

### 入群后检测

入群检测使用独立的 `join_gate.base` 和 `join_gate.groups`，共享配置及群级覆盖的合并方式与消息策略一致。示例包含 `question_groups.join` 的五道题，分别判断宣传、金钱、赚钱承诺、色情灰产业务和二次元性内容；二次元项仅用于观察。消息检测使用 17 道选题，入群检测单独选择 `join.*`。

默认 `enabled: false`、`mode: "record"`。已有配置可以从 `examples/scoring.json` 补入 `question_groups.join` 和 `join_gate`，校验后重载。省略整个 `join_gate` 时保持关闭；开启命令会提示先补齐配置。两个检测区域的模式、评分和队列分别管理；只读采集群应在 `join_gate.groups` 显式保留 `mode: "record"`。

```json
"join_gate": {
  "base": {
    "enabled": false,
    "mode": "record",
    "trigger": "member_joined",
    "queue_limit": 100,
    "questions": ["join.*"],
    "min_confidence": 0.75,
    "on_low_confidence": "abstain_question",
    "scoring": {
      "initial": 0,
      "rules": [{
        "name": "入群资料宣传金钱或色情",
        "when": "p.join.promotion >= 0.85 && (p.join.money >= 0.85 || p.join.sexual >= 0.85)",
        "add": 100
      }]
    },
    "condition": "score >= 100",
    "rules": {
      "remove": {"enabled": true, "count": 1, "action": "kick"},
      "delete_join_message": {"enabled": true, "count": 1, "action": "delete"}
    }
  },
  "groups": {"-1001234567890": {"enabled": false, "mode": "record"}}
}
```

机器人需要群管理员身份才能收到成员变更事件，执行移出还需要限制成员的权限。新进程明确订阅 `chat_member`，从成员状态由群外变为群内的事件识别新加入者；目标数字 ID 取自发生状态变化的成员。邀请人的身份和入群服务消息继续完整归档。权限调整及重复投递保持同一次入群的语义，离群后再次加入形成新记录。

每次入群的模型材料为 `{"member":{"name":"名字 姓氏","bio":"简介","bio_status":"available"}}`。姓名来自入群事件，bio 使用现有 Bot API 按成员数字 ID 重新查询并验证身份；资料为空或获取失败时 `bio` 为 `null`，获取状态和查询错误留档。判断依据是该次入群取得的姓名和 bio；资料中的链接保留为 bio 文本。资料快照和模型输入一经保存，后续重试继续复用。

默认计分为宣传概率至少 85%，同时金钱或色情概率至少 85%，加 100 分，最终条件 `score >= 100`。赚钱承诺题独立保存结果。`noul` 概率、服务端置信度和逐题弃答规则与消息检测相同。入群动作支持单次 `kick`，含义为移出群并允许重新加入；独立的 `delete_join_message` 规则负责删除匹配到违规入群的系统提示。

入群提示按群 ID、新成员数字 ID、Telegram 秒级时间和唯一入群记录关联。提示先到、判定先到都可处理，删除依据保留对应入群记录与模型尝试 ID。普通成员的提示保留；多人共用一条提示时，只有其中全部成员均有对应的违规判定才规划删除。时间不一致、成员未知或同秒多次入群等关联不明确的提示继续保留。

清理沿用入群检测的开关与模式，提示接收时和违规判定所用的删除规则须一致；任一来源为 `record` 时保留观察记录。删除与移出分别留档，已离群成员仍可清理其原来的违规入群提示。执行前复核当前配置、原消息版本、关联证据和受保护身份。Telegram 删除窗口为 48 小时，机器人还需要相应的删除消息权限；超时、权限不足及接口失败均记录结果。原始服务消息永久保存在数据库。

单独关闭清理可在文件中设置 `join_gate.base.rules.delete_join_message.enabled: false`，或在 `join_gate.groups` 为目标群覆盖这一字段，然后由所有者执行 `/as_reload`。`/as_join off` 同时停止入群检测及待发送的提示删除；关闭或切回记录模式后，迟到的旧提示保持保留状态。

群管理员使用：

```text
/as_join on
/as_join status
/as_join mode enforce
/as_join mode record
/as_join off
```

`on` 保留当前模式；首次开启采用默认记录模式。`off` 会关闭当前群的待判和正在推理的入群任务，并取消尚未派发的动作；迟到的模型结果继续归档。已开始发送的请求先完成，再确认配置修改。记录模式接收的入群保持观察性质。群主、管理员、机器人自身和配置中的所有者受保护；发送前重新检查当前配置、成员状态及该次入群是否仍有效，已有封禁保持原状。

入群失败重试使用独立退避，待判容量按群保留最新 N 条。原始事件、被淘汰任务、资料查询、全部模型尝试、逐题结果和评分永久保留。进程重启会恢复未完成的推理；已派发且结果未确认的动作标记为 `uncertain`，供核查。

本功能包含成员事件订阅和数据库升级，需要重启新二进制。当前数据库版本为 10，配置默认保持关闭，重启后在目标群使用 `/as_join on` 开始记录。

## 消息材料与模型协议

原始 Telegram 更新及消息 JSON 完整留档。发给模型的 `state` 使用三个顶层字段：

- `sender`：发言者名字、用户名、机器人标记、可获取的简介和频道介绍；简介文本缺失时 `bio` 为 `null`。简介中 Telegram 链接的目标资料放在 `links`。消息上下文 v4 对用户补入数字 ID、Premium 标记、头像和自定义状态表情；`personal_channel` 单独存放用户主动挂在主页的频道、订阅数、频道介绍、头像及最新一帖的文字／配文、日期和照片。简介链接与主页频道分别按真实身份解析，即使显示名称相同也保持独立。在线与最后活动时间不采集。
- `reply`：被回复消息的作者、正文、配文、按钮、链接与媒体说明；没有回复时为 `null`，引用保留一层。
- `message`：直接使用数组，按原顺序保存 `text`、`mention`、`link` 片段；配文片段带 `source: "caption"`。按钮、媒体和来源分别使用 `buttons`、`media`、`source` 片段。提及对象或 Telegram 链接的资料挂在对应的 `profile` 上。

同文案、同目标的按钮合并为 `text`、`link`、`count`；地址在按钮片段的 `links` 中存一份，目标资料放在按链接编号索引的 `profiles`。当前消息的隐藏链接展开为保留可见文字与实际地址的 `link` 片段，正确处理 Telegram 使用的 UTF-16 偏移量；引用区域继续使用原来的独立链接编号。
模型材料保留音视频时长、文件名、标题、表演者等可见说明。正文或配文的自定义 emoji 附带对应文字、UTF-16 偏移和长度，普通 Unicode emoji 保持文本。贴纸和自定义 emoji 尽量补入对应表情、类型、贴纸包短名和展示标题；Bot API 没有贴纸包简介字段。文件传输 ID 和未知 API 字段留在归档中。

简介通过额外查询获取并短期缓存，获取状态记录在诊断日志中；Telegram 返回的资料可用性受其接口限制。
当前消息的照片、静态贴纸、视频／动画贴纸的静态缩略图，以及 GIF 动图的首帧和末帧会补入图片资产供模型判断。普通视频、音频、其他文档及回复中的附件继续保留现有文字和元数据；相册中的各条消息分别处理。
实际收到的其他机器人群消息、用户经由 inline bot 发布的消息均参与检测，原始 `from.is_bot` 标记和来源信息继续提供给模型。管理员身份与频道自动同步的跳过规则同样适用；经过权限验证的管理命令继续走管理流程。Telegram 决定具体的消息投递范围。
更新程序后，新收到的其他机器人消息进入待判队列；历史上标为 `ignored` 的记录保持原始归档状态。

十七题示例分别观察当前消息、发言者资料、首个有效目标资料及调用意图；处置使用消息／发言者资料两项组合计分，以及独立的灰产招揽计分。二次元性内容的结果仅用于观察。多个目标分别挂载资料，目标题统一选同一个首个有效目标，便于核对判定归属。省略所有题库定义时，内建传统五题仍可使用。
顶层 `questions` 或 `question_groups` 可在配置中修改或扩展，支持 `noul`、`choice`、`score`，每题包含 `instructions`。`noul` 的 `criteria` 可省略；提供时包含 `true`、`false` 两项说明。`choice` 包含 2–255 个选项；`score` 包含 2–10 个从低到高的等级说明。
客户端发送 `state`、`model`、已选 `questions`，带图时增加顶层 `assets`。保留题目标准，验证模型名、答案类型、完整性、概率分布和等级范围。合法答案的低置信度按逐题弃答处理，图片校验错误支持一次文字降级，其他失败沿用模型重试。
所有问题共同使用这条消息的材料，各题独立作答。

每次编辑创建新版本并重新排队。队列中保留的版本逐一判断；旧版本结果用于留档，最新版本负责触发统计和处罚。
累计统计中同一条消息对同一规则计一次；窗口基于接收时间。只记录模式与执行模式的累计历史分别计算。
改变题目或概率门槛会开启独立的累计系列。

### 图片与贴纸：SORS assets 协议

图片放在顶层 `assets`，当前消息对应的 `photo` 或 `sticker` 项通过 `image` 字段引用。附加图片保留原有对象与数组的遍历顺序，维持发言者资料、引用、当前消息的排列；文字降级恢复原有材料排列和数值表示。每张图生成独立 UUID，字节使用标准、带补位、无换行的 Base64；`state` 内的 data URI 按普通文字处理。带图请求同时发送 `TypeSafe-Danger-Zone-Images: true`。以下示例省略真实 Base64 数据：

```json
{
  "model": "your-served-model-name",
  "state": {
    "message": [{"type": "media", "media": [{
      "type": "photo", "image": "30b6c56f-5218-4c05-800a-853686ad45ec"
    }]}]
  },
  "questions": {
    "content.promotion": {"type": "noul", "instructions": "当前消息的图片是否在宣传或招揽？"}
  },
  "assets": {
    "30b6c56f-5218-4c05-800a-853686ad45ec": {
      "type": "image", "format": "png", "data_base64": "..."
    }
  }
}
```

照片优先选择 Telegram 提供的最长边至多 1024 的最大尺寸；只有更大尺寸时选择较小的一档。静态贴纸读取原图，视频／动画贴纸读取静态 `thumbnail`，兼容旧字段 `thumb`，并在模型材料标记 `image_source: "thumbnail"`。转发消息自身的图片属于当前消息，回复中的图片保持元数据形式。

### 动图：首帧与末帧

Telegram 把 GIF 动图以 `animation` 下发，文件通常已转成无声 MP4；直接作为文件发送的 `image/gif` 文档同样处理。动图消息附带的 `document` 镜像项保持元数据形式。程序下载一次完整文件，取首帧和末帧各一张，挂在 `animation`（或 GIF 文档）媒体项的 `frames` 数组中，每项带 `position` 和 `image` 引用：

```json
{"type": "animation", "mime_type": "video/mp4", "duration": 4, "frames": [
  {"position": "first", "image": "8c0a…"},
  {"position": "last",  "image": "f31e…"}
]}
```

首末两帧标准化后完全相同（如单帧 GIF）时合并为一项 `position: "single"`。GIF 在进程内按帧处置方式合成整幅画面后取帧；解码前先扫描文件结构，帧数超过 1000 或各帧声明像素合计超过 64 Mi 时放弃。MP4 交给 `ffmpeg` 解码：输入写入私有临时目录，只允许读本地文件，像素上限 16 Mi，限时 8 秒，子进程环境为空，结束后删除临时文件。两帧都经过与照片相同的标准化。文件超过 4 MiB 时改用 Telegram 提供的静态缩略图，标记 `image_source: "thumbnail"`。抽帧失败或找不到 `ffmpeg` 时，与其他图片失败一样保留文字材料并记录错误。一条动图在下载预算中算一个图像位置。

通过现有 Bot API `getFile` 获取文件路径，再使用同一 Telegram 服务下载。v4 每条消息最多尝试六个图像位置，优先当前消息媒体、状态表情和正文／配文的自定义 emoji（最多三个），其次为主页频道帖子和头像；共用 10 秒下载预算。单张输入和标准化结果均限制在 4 MiB。解码前检查尺寸，原始图片限制在 16 Mi 像素、单边 16384 像素以内。PNG、JPEG 和 WebP 按实际文件格式解码，EXIF 方向落实到像素，透明区域铺白底，最长边缩至 1024 以内，较小的图放大或补边至每边至少 280，最终统一编码为 PNG。下载和格式处理失败时继续使用已取得的文字材料，并保存查询状态及错误。

服务端需要已加载视觉编码器，`model.name` 与 `GET /v1/models` 返回的名称保持一致。消息检测和入群检测共享串行模型调用通道，等待可以随进程停止取消。纯文字请求使用 `model.timeout_seconds`，带图请求使用独立的 `model.image_timeout_seconds`（默认 60 秒，允许 1～3600 秒）。

可确认与图片、资产或展开大小有关的 HTTP 400/413，以及图片相关的 422、明确的图片展开 token 超限，会单独归档原请求与响应后追加一次纯文字请求。模型名、鉴权及服务故障保持常规模型失败和退避行为。文字降级结果可继续计分，图片缓存只采用实际成功看过相同图片的判定。

新消息使用上下文版本 3。图片下载证据、原始有效图片字节、标准化资产及 UUID 引用与首次模型上下文一起冻结，重试复用同一份内容。旧版已冻结上下文及版本 1／2 的待判消息继续使用原格式。

### 黑灰产业务招揽题

`content.illicit` 针对当前消息的文字和图片，判断是否主动提供、推广或招揽查人／查手机号／个人信息查询／开盒、现实色情业务、赌博博彩、毒品交易、洗钱或跑分。免费体验、引导私聊或进群办理同样属于招揽；二次元虚构作品本身的分享和售卖归动漫观察类。动漫配图所推广的其它业务仍按实际业务判断。普通讨论、合规开户、新闻、反诈、举报和寻人求助归为正常。发言者资料与目标资料保持各自题组的归属。

```json
{"name": "消息招揽黑灰产业务", "when": "p.content.illicit >= 0.85", "add": 100}
```

这一项独立计分，最终条件仍为 `score >= 100`，实际动作继续由当前群的 `record`／`enforce` 和动作开关控制。配置含群级 `questions` 或 `scoring.rules` 覆盖时，应同步加入新题和计分项；题数与计分数组分别按已有覆盖规则替换。

### 性内容的两类判断

`content`、`profile`、`target` 和 `join` 四组分别保留两项结果：

- `sexual_anime`：二次元动漫、漫画、插画、虚构角色的性相关表现或同人内容，结果仅记录。
- `sexual`：现实色情灰产业务招揽，例如真人色情资源销售、招嫖、付费裸聊或色情交易引流；沿用原有 `sexual` 键及计分位置。

二次元观察项的分数贡献为 0。原有金钱推广及其他黑灰产业务规则继续生效；带动漫配图的赌博、洗钱、开盒广告仍按业务判断。目标资料题组继续整体保持观察性质。所有计分公式、概率门槛、名单逻辑及处罚开关均沿用现有配置。

已有明确选题列表需要加入对应的 `sexual_anime`；示例已同步消息、发言者和目标题，入群的 `join.*` 自动包含新观察项。配置重载后，新消息采用新题义；既有原文、旧答案和策略快照保留，缓存与待执行动作沿用题目版本校验。

### 提及对象与 Telegram 链接资料

资料来源使用现有 Bot API `getChat`：公开群／频道按 `@用户名` 查询，已观察到的 bot 可使用归档数字 ID。支持正文、配文、隐藏链接、按钮及发言者简介中的 Telegram 用户名／公开链接。直接链接读取目标群／频道资料；提及对象的 bio 中再补一层 Telegram 链接资料。资料保持来源归属，网页抓取和额外依赖属于本次选择之外。

`t.me/+…`、`t.me/joinchat/…` 邀请链接记为 `unsupported`；查询失败记为 `unavailable`，每次结果保留查询参数、SDK 解析后的返回值、错误、时间和所关联消息版本。目标缺失时片段内 `profile` 为 `null`，原消息继续正常模型判断。目标资料提供判断背景，实际处罚身份继续来自 Telegram 原始消息与可信调用关联。

每条消息最多发起 8 次新资料查询，总预算 10 秒。重复引用共享查询；成功查询沿用 `telegram.profile_cache_seconds`，上限一小时，失败缓存最多一分钟。超出单条预算的查询记为 `limit`，后续消息仍有自己的查询机会。分段结构自版本 2 引入，当前版本 3 在原结构上补充图片引用；旧消息已冻结的上下文及旧待判版本继续使用原格式。

请求示例中的消息区域：

```json
{
  "message": [
    {"type": "text", "text": "看看 "},
    {"type": "mention", "text": "@examplechannel", "profile": {"username": "examplechannel", "name": "示例频道", "bio": "公开简介", "kind": "channel"}}
  ]
}
```

## 广告内容指纹缓存

模型成功判断广告后，消息内容能够独立满足最终判定条件的结果会进入 SQLite 指纹库。同样的广告换 bot、换调用者、换群再次出现时，可以复用内容答案，并按新消息的策略快照重新计分、累计和规划动作。

- 内容 SHA256 基于规范化正文、配文、按钮文案与实际链接、引用／代码等格式实体、媒体稳定标识及发送者类别。bot、用户和频道分别匹配；作者身份、消息 ID、群 ID、时间和调用者单独留在原始归档中。媒体身份缺失或材料解析异常时继续走常规模型路径。
- 联合匹配内容指纹、模型 ID、模型服务地址和判定配置指纹。判定配置包括选中的完整题目、计分项、最终条件及置信度设置；群模式和处罚参数保持各群独立控制。同名模型替换权重时应同时更新模型版本标识。
- 当前支持按约定判断 `.message` 当前消息文字和图片的 `content.*` 题目跨身份复用；自定义题目须遵守这个范围。`profile.*`、`target.*` 等其他区域的答案在命中记录中保持 `unknown`。资料单独触发、资料可能扣分或改变最终条件等无法证明内容独立达标的情况继续调用模型。
- 命中记录保存源模型尝试 ID、独立的计分结果及新消息版本。每条新消息的完整原文照常归档，真实模型调用只计入 `attempts`，缓存命中单独计入 `content_cache_matches`。

入库时的缓存命中直接完成判定，模型退避期间仍可处理已知广告，并保留待判队列的原有容量。`record` 群继续只记录；`enforce` 群执行前重新核对当前模式、权限、消息版本及缓存的模型／规则版本。广告 bot 实际删除成功后继续自动加入 bot 黑名单，调用消息与调用者封禁使用本次消息的可信关联。

分段上下文的缓存指纹同时包含上下文版本和目标资料快照。可查询的提及／链接会先取得资料，再查内容缓存；查询失败的目标保持常规模型路径。Bot API 范围外的邀请链接保留稳定的空资料，纯广告正文仍可复用。每个消息版本冻结自己的资料，重试沿用已存快照。

含照片、贴纸或动图的新消息先取得图片，指纹在原有媒体稳定标识之外加入标准化图片实际字节的 SHA256，动图的每一帧都计入。随机 UUID 通过图片格式和哈希规范化，同一图文材料可跨引用串复用。图片缺失、部分下载失败及文字降级保持常规模型路径；缓存发布时还会核对成功请求实际携带的图片与冻结材料一致。每个命中新消息仍保存自己的图片资产和来源查询。

新二进制重启后数据库升级到版本 10，既有指纹、消息和推理记录保持原样。调试日志事件 `content_cache.hit` 包含指纹条目 ID、源模型尝试 ID 和模型 ID；`reference.lookup` 包含资料查询 ID、状态、缓存标记及展开层数。

## 异常、恢复与留档

模型超时、不可用或返回异常时，原消息与失败记录入库，待判任务按 `model.retry_seconds` 的间隔重试。
每群队列满时，新版本挤掉最旧的待判版本；淘汰状态为 `evicted`，原始记录继续保留。
模型调用中的版本也可能被淘汰，它随后返回的结果保留为证据。
模型恢复后继续处理剩余任务。

每个消息版本保存接收时的题目和策略快照，首次查询到的资料随模型输入保存。
已保存的旧版模型输入继续原样复用；升级后的新材料使用三个来源字段。材料构建异常也会保存失败尝试并按重试间隔调度。
跳过原因保存在 `revisions.snapshot.skip_reason`。旧版已经入队的消息会在模型调用前重新检查管理员与自动同步条件；跳过时保留原始消息、旧尝试记录及策略，关闭本次待判工作。已排队的自动同步消息处罚也会在发送前取消。
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
| `bot_blacklist` | 全局 bot 身份、当前状态、备注、名单代次及移除时的版本边界 |
| `bot_blacklist_events` | 自动学习、历史导入、所有者增删改的审计；编辑保存前后值，管理命令按 update ID 去重 |
| `blacklist_matches` | 每个版本命中的 bot ID 与名单代次，完整原文仍保存在 `revisions.raw` |
| `action_evidence` | 名单删除、关联调用删除或调用者封禁的依据，连接源删除动作与名单代次 |
| `bot_action_events` | 名单／关联动作的计划、证据切换、模式升级及最终结果，保留旧意图快照 |
| `bot_invocations` | Telegram 提供的调用关联、调用版本指纹和目标删除动作；支持调用消息稍后入库 |
| `bot_invocation_actions` | 每份调用证据分别关联删除和封禁动作，支持多来源及独立去重 |
| `content_cache` | 广告内容 SHA256、规范化材料、模型 ID／服务地址、判定配置指纹、内容答案与源模型尝试 |
| `content_cache_matches` | 每个缓存命中版本及其源模型尝试、重新计算的答案采用状态／计分／最终条件 |
| `reference_profiles` | Bot API 资料查询的参数、解析后的返回值、结果状态、错误和时间；原文消息仍独立留档 |
| `revision_reference_profiles` | 消息版本与首次选定资料查询结果的关联，记录展开层数 |
| `join_events` | 成员变更原文、入群身份、独立策略与选题快照、bio 查询参数／返回／错误、冻结上下文及队列状态 |
| `join_attempts` | 入群检测的每次模型请求、完整响应、原始答案、逐题采用状态、计分、错误和耗时 |
| `join_actions` | 每次入群的拟移出动作、记录／待执行状态、最终结果及跳过原因 |
| `join_service_actions` | 违规入群提示的删除计划、原消息版本、入群／模型尝试证据、模式及实际结果；接收时的入群策略保存在对应 `revisions.snapshot.join` |
| `revision_images` | 每个消息版本冻结的 SORS 图片资产，以及 getFile 参数、解析返回、下载状态、有效原始图片字节、标准化哈希和引用的审计 |

日志写入标准错误，使用 JSON 格式。令牌和 API 密钥通过环境变量读取。
数据库从旧版本自动升级到版本 10，既有消息、模型尝试、队列和动作记录全部保留。版本 2 增加计分审计，版本 3 增加全局黑名单与调用关联审计，版本 4 增加备注与编辑前后值，版本 5 增加同一调用的多动作关联，版本 6 增加广告内容指纹与命中审计，版本 7 增加引用资料查询与关联，版本 8 增加入群检测及处置记录，版本 9 增加入群提示清理审计，版本 10 增加冻结图片资产与下载审计；升级前先备份数据库。
数据库包含群消息和用户资料，文件权限为 `0600`，默认持续保留全部记录。
运行中备份请使用 SQLite 的备份能力；停机复制时一并保留数据库及尚存的 WAL 文件。

## 调试日志

本群管理员使用 `/as_set debug true` 开启、`/as_set debug false` 关闭，`/as_reset debug` 恢复共享继承；所有者使用 `/as_set base debug true` 修改共享默认值。文件修改经 `/as_reload` 生效。开关按当前配置逐条检查，正在进行的请求也会采用更新后的设置。

日志为逐行 JSON。`DEBUG` 用于详细诊断；配置修改、人工标注和处罚结果保留 `INFO` 记录，模型失败保留 `WARN`，严重错误保留 `ERROR`。只记录模式同样输出完整的判定诊断，拟执行动作的状态为 `recorded`。

用 `chat`、`message`、`revision`、`update` 串联同一条消息的处理；结果保存后附带模型尝试 ID `attempt`，处罚日志附带 `action_id`。首次接收时用 `update` 关联，入库后补充 `revision`。

| `event` | 主要内容 |
| --- | --- |
| `message.received` / `message.archived` | 收到的消息与编辑标志、机器人及自动同步标记、是否为管理命令、字节数、入库状态、跳过原因、重复更新、选题数量 |
| `membership.checked` / `membership.failed` | 群成员身份查询结果、耗时与异常 |
| `model.skipped` | 已领取任务因管理员身份、频道自动同步等原因跳过模型，及状态是否成功写为 ignored |
| `queue.evicted` / `queue.resized` | 队列容量、实际淘汰数、触发原因；启动恢复和修改容量也会记录 |
| `profile.started` / `profile.completed` / `profile.cache_hit` | 资料查询耗时、获取状态、缓存命中，以及是否获得简介或说明 |
| `image.lookup` | 当前图片／贴纸预览／动图的下载状态、标准化尺寸（动图为首帧）、耗时和安全处理后的错误 |
| `model.image_fallback` | 带图请求被拒后的独立失败尝试 ID，以及追加文字判断的原因 |
| `context.started` / `context.ready` / `context.failed` | 判断材料是否复用数据库快照、材料字节数、构建异常 |
| `model.claimed` / `model.started` / `model.completed` | 排队时间、模型名、题目列表、图片数量、当前请求实际采用的超时、调用耗时、请求及响应字节数 |
| `model.answers` | 各题概率、服务置信度、有效或弃答状态与原因 |
| `policy.scored` | 总分、各加减分项的命中／跳过情况、最终条件表达式及三值结果 |
| `policy.rules` | 处置规则的实际累计次数、要求次数、时间窗口、消息是否首次计数、未达到条件的原因 |
| `model.archived` | 保存后的版本状态和原因，可区分正常完成、失败待重试、版本被替代及队列淘汰 |
| `model.failed` / `model.retry_scheduled` | 模型错误、实际版本状态和下一次模型重试时间；退避作用于模型处理循环 |
| `action.planned` | 拟执行动作 ID、命中规则、只记录或待执行状态、禁言截止时间 |
| `action.preflight` / `action.membership` / `action.dispatching` / `action.completed` | 执行前复核、成员状态、开始调用、实际结果，以及保护管理员、配置变更或消息编辑等跳过原因 |
| `config.updated` / `config.reset` / `config.reloaded` / `config.rejected` | 配置作用域、操作人、参数路径、成功操作及拒绝原因；配置值留在配置文件和命令记录中 |
| `blacklist.matched` / `blacklist.added` / `blacklist.updated` | 名单命中、成功删除后的自动加入、所有者手动变更；包含 bot ID 和审计关联 |
| `invocation.planned` | 调用消息 ID、目标版本、关联删除动作与源广告删除动作 |
| `command.authorization` / `annotation.recorded` | 命令权限检查、人工标注的目标消息与标签 |
| `join.received` / `join.profile` / `join.answers` | 入群阶段、开关、资料查询状态及各题结果；由独立入群策略的 `debug` 控制 |
| `join.evaluated` / `join.model_failed` | 入群检测的总分、条件、模式、任务状态或失败重试时间 |
| `join.config_updated` / `join.action_completed` | 入群配置修改和实际移出结果，用 `chat`、`user`、`join` 关联 |
| `join.service_deleted` | 入群提示删除的成功、失败或跳过结果，含原消息 ID、动作 ID 与关联的入群／模型尝试证据 |

常规诊断输出阶段元数据和判定结果，消息正文、个人简介、完整请求和模型响应继续通过数据库留档。Telegram 错误中的 bot Token 会脱敏。空闲轮询保持静默。

下面是只记录模式的日志示意，省略部分字段：

```json
{"level":"DEBUG","event":"model.started","chat":-1001234567890,"message":42,"revision":100,"question_count":8,"timeout_seconds":20}
{"level":"DEBUG","event":"policy.scored","chat":-1001234567890,"message":42,"revision":100,"attempt":101,"score":100,"condition_result":"true"}
{"level":"DEBUG","event":"action.planned","chat":-1001234567890,"message":42,"revision":100,"attempt":101,"action_id":12,"action":"delete","status":"recorded"}
```

直接运行时，终端可实时看到日志；也可以追加到本地文件：

```sh
mkdir -p logs
chmod 700 logs
umask 077
./antispam -config config.json 2>>logs/antispam.jsonl
```

Compose 部署使用 `docker compose logs -f antispam` 查看。日志字段中的数值是实际计分和持久化结果；模型识别准确率仍需结合人工标注评估。

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

容器测试覆盖非 root 默认用户、HTTPS 根证书、只读根文件系统下以 UID 65532 运行 `ffmpeg` 抽帧、命令写回、数据库持久化和重启恢复；同时通过仓库中的 Compose 定义运行同一组流程。

测试使用本地 HTTP 服务和真实 SQLite，覆盖 API 协议、原始字段保存、配置权限、逐版本处理、异常积压、恢复和处罚。
正式启用前在测试群核对机器人权限、资料获取情况及模型实际效果。
