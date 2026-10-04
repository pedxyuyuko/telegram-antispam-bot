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
文件中的 `base.rules` 和 `questions` 一旦显式提供，就定义完整集合；省略时使用内建默认值。
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

## Telegram 管理命令

群内命令可加上 `@机器人用户名` 后缀。普通群成员的命令形状消息继续参与检测。

| 命令 | 权限与作用 |
| --- | --- |
| `/as_help` | 显示帮助；也支持私聊 `/start` |
| `/as_config` | 群管理员：查看本群最终配置及本群覆盖项 |
| `/as_set mode "record"` | 群管理员：切换本群模式，启用处罚时改为 `"enforce"` |
| `/as_set queue_limit 50` | 群管理员：修改本群待判容量，缩小时立即淘汰最旧待判版本 |
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

## 消息材料与模型协议

每份判断材料含发言者名字、用户名、可获取的简介，以及完整的 Telegram 消息 JSON。
正文、媒体配文、格式化实体、隐藏链接目标、按钮文字与链接、inline bot 来源、转发、回复和其他 API 字段均保留。
额外展开格式化实体的可见文字，正确处理 Telegram 使用的 UTF-16 偏移量。

简介通过额外查询获取并短期缓存，查询结果附带获取状态；Telegram 返回的资料可用性受其接口限制。
媒体消息保留 API 已提供的文字和元数据；图片、二维码、音视频内容识别属于后续范围。
Telegram Bot API 对其他机器人直接发送的群消息存在接收限制，用户经由 inline bot 发布的消息属于检测范围。

默认问题是：推广意图、诈骗诱导、黑灰产招揽、正常讨论语境、最终违规判断。
`questions` 可在配置中修改或扩展，每题使用 `type: "noul"` 和 `instructions`。
客户端发送 `state`、`model`、`questions`，使用 `answers.<题目>.noul` 作为概率，并验证模型名、答案完整性和 `[0,1]` 范围。
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
| `attempts` | 每次模型请求、完整响应、概率、错误和耗时 |
| `hits` | 按消息去重的规则触发历史 |
| `actions` | 拟采取的动作、实际执行状态及错误 |
| `annotations` | 人工标注历史及原消息快照 |

日志写入标准错误，使用 JSON 格式。令牌和 API 密钥通过环境变量读取。
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
