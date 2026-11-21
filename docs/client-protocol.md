# 双端协议

## HTTP 注册、登录与会话认证

Unity 向网关发送 `POST /v1/register`，正文为 `application/protobuf` 编码的 `RegisterRequest`。账号须为 1 至 64 个 UTF-8 字节且首尾无空白。密码至少 8 个字符，最多 72 个 UTF-8 字节。登录服务将账号和 bcrypt 密码摘要写入 MySQL。创建成功返回 `201` 和空正文，随后客户端可登录获取票据。输入无效返回 `400`，媒体类型错误返回 `415`，账号已存在返回 `409`。设置正数 `SLG_REGISTER_RATE_PER_SECOND` 后，越过每秒限额返回 `429` 和 `Retry-After: 1`。注册并发达到 `SLG_REGISTER_CONCURRENCY` 或登录服务不可用时返回 `503`。

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

认证后仍使用同一连接。客户端发送 `WorldRequest`（消息 ID 3），网关返回序号相同的 `WorldReply`（消息 ID 4）。只读请求可携带已显示的实例和修订号，两者均匹配时回包省略快照。重连可发送空查询，获取当前完整地图和自己的出生位置。

提交行军时携带当前实例、目标格 ID 和 `command_id`。断线后用同一账号重新登录时，客户端先读取快照。如果仍是原世界实例，客户端使用原指令 ID 重试未确认的行军。C++ 游戏服从自己的相邻地块选择起点，预留目标，约 3 秒后在下一次读写前提交占领。回包中的服务器时间和到达时间用于显示倒计时。

试玩地图最多容纳两个账号。游戏服收到新请求时，会回收连续 60 秒未请求世界的账号名额、地块和行军预留，其他玩家保留进度。回收名额时更新世界实例，属于旧实例的指令需要重新选择目标。满员时客户端每隔 5 至 8 秒复查名额。
