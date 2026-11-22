# 双端协议

## HTTP 注册、登录与会话认证

Unity 向网关发送 `POST /v1/register`，正文为 `application/protobuf` 编码的 `RegisterRequest`。注册时账号和密码均须为 1 至 8 个 Unicode 字符，账号首尾无空白。登录服务将账号和 bcrypt 密码摘要写入 MySQL。创建成功返回 `201` 和空正文，随后客户端可登录获取票据。输入无效返回 `400`，媒体类型错误返回 `415`，账号已存在返回 `409`。设置正数 `SLG_REGISTER_RATE_PER_SECOND` 后，越过每秒限额返回 `429` 和 `Retry-After: 1`。注册并发达到 `SLG_REGISTER_CONCURRENCY` 或登录服务不可用时返回 `503`。

1. Unity 向 Go 网关发送 `POST /v1/login`，正文是 `application/protobuf` 编码的 `LoginRequest`。网关通过 etcd 找到登录服务并转发请求。登录服务查询 MySQL、校验密码哈希，再将有效期 30 秒的票据写入 Redis。网关在 `LoginReply` 中返回票据和自己的 TCP 端口。账号或密码错误返回 `401`，登录服务或存储不可用返回 `503`。
2. Unity 连接登录地址的主机与返回的 TCP 端口，发送 `ConnectRequest`（消息 ID 1），携带票据和协议版本 1。网关用 Redis `GETDEL` 原子地读取账号并消耗票据，然后返回携带账号与会话 ID 的 `ConnectReply`（消息 ID 2）。使用相同 Redis 和票据前缀的网关实例可以共同完成这两步。
3. 客户端收到与请求序号匹配的认证回执后，视为连接成功。票据无效、过期、已使用或协议版本不匹配时，网关关闭连接。当前 TCP 读空闲超时为 15 秒。

Unity Web 使用网页所在站点的 `POST /v1/login`，随后连接同源 `/v1/session` WebSocket。HTTPS 页面选择 WSS。连接请求、回复和 Redis 票据沿用原生客户端的消息定义，WebSocket 每个二进制消息携带一个完整 Orbit 帧。网关将该入口挂载到已有 HTTP 端口，使用 Orbit 的同源校验。

Orbit 帧头为 10 字节大端序，依次是 payload 长度 `uint16`、消息 ID `uint16`、请求序号 `uint32`、保留位 `uint16`。长度不含帧头，保留位为 0，登录联调的单帧 payload 上限为 1024 字节。TCP 是字节流，接收端先读满帧头，再按长度读满正文。

当前默认监听本机明文 HTTP 和 TCP。公网部署时分别为登录入口和 TCP 连接启用 TLS。

## 生成 Go 和 Unity 类型

[proto/session.proto](../proto/session.proto) 和 [proto/world.proto](../proto/world.proto) 定义双端协议。用 `protoc 31.1` 和 `protoc-gen-go v1.36.9` 在服务端仓库根目录执行以下命令。脚本默认将 C# 输出到相邻的 `client` 目录，可通过 `SLG_CLIENT_DIR` 指定其他检出位置。工具路径可用 `PROTOC` 和 `PROTOC_GEN_GO` 覆盖。

```sh
sh scripts/generate-proto.sh
```

## 共享地图与行军

认证后仍使用同一连接。客户端发送 `WorldRequest`（消息 ID 3），网关返回序号相同的 `WorldReply`（消息 ID 4）。只读请求可携带已显示的实例和修订号，两者均匹配时回包省略快照。支持增量同步的客户端还携带 `known_battle_id`，首次传 0。此时地图修订号只反映公共状态，`available_troops` 在回包中单独提供。省略战报游标的旧客户端仍通过快照读取己方兵力和最近一份战报。

提交行军时携带当前实例、目标格 ID、`command_id` 和出征兵力 `troops`。省略兵力时使用 40。相同指令 ID 的重试须沿用原目标和兵力。游戏服从己方相邻地块选择起点，预留目标并扣减预备兵力。预备兵力初始为 100，低于 100 时每秒补充 5，自动补充最多到 100。撤回的存活部队全数加入预备兵力，可使总数超过 100。出生地受保护。

行军约 3 秒后抵达。中立地块由出征士兵直接占领，敌方地块交由战斗服结算。进攻胜利时存活部队留作驻军。防守获胜时，剩余守军继续驻守，存活的进攻部队返回预备兵力。战斗服暂不可用时，部队保留在等待结算状态，游戏服用原快照和种子重试。回包的 `battle_reports` 按战斗 ID 升序返回游标之后的战报，服务端保留最近 8 份。实例变化后从最近 8 份重新同步。

试玩地图最多容纳两个账号。连续 60 秒未请求世界后可以回收名额、地块和中立行军预留。敌对行军自受理起保留双方名额和驻军，待相关战斗全部结算后才回收仍然空闲的名额。战报在名额回收后仍可读取。回收名额时更新世界实例，属于旧实例的指令需要重新选择目标。满员时客户端每隔 5 至 8 秒复查名额。
