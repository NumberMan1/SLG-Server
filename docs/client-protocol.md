# 双端协议

## HTTP 登录与会话认证

1. Unity 向 Go 网关发送 `POST /v1/login`，正文是 `application/protobuf` 编码的 `LoginRequest`。网关通过 etcd 找到登录服务并转发请求。登录服务查询 MySQL、校验密码哈希，再将有效期 30 秒的票据写入 Redis。网关在 `LoginReply` 中返回票据和自己的 TCP 端口。账号或密码错误返回 `401`，登录服务或存储不可用返回 `503`。
2. Unity 连接登录地址的主机与返回的 TCP 端口，发送 `ConnectRequest`（消息 ID 1），携带票据和协议版本 1。网关用 Redis `GETDEL` 原子地读取账号并消耗票据，然后返回携带账号与会话 ID 的 `ConnectReply`（消息 ID 2）。使用相同 Redis 和票据前缀的网关实例可以共同完成这两步。
3. 客户端收到与请求序号匹配的认证回执后，视为连接成功。票据无效、过期、已使用或协议版本不匹配时，网关关闭连接。当前 TCP 读空闲超时为 15 秒。

Unity Web 使用网页所在站点的 `POST /v1/login`，随后连接同源 `/v1/session` WebSocket。HTTPS 页面选择 WSS。连接请求、回复和 Redis 票据沿用原生客户端的消息定义，WebSocket 每个二进制消息携带一个完整 Orbit 帧。网关将该入口挂载到已有 HTTP 端口，使用 Orbit 的同源校验。

Orbit 帧头为 10 字节大端序，依次是 payload 长度 `uint16`、消息 ID `uint16`、请求序号 `uint32`、保留位 `uint16`。长度不含帧头，保留位为 0，登录联调的单帧 payload 上限为 1024 字节。TCP 是字节流，接收端先读满帧头，再按长度读满正文。

当前默认监听本机明文 HTTP 和 TCP。公网部署时分别为登录入口和 TCP 连接启用 TLS。

## 生成 Go 和 Unity 类型

[proto/session.proto](../proto/session.proto) 是协议定义的唯一来源。用 `protoc 31.1` 和 `protoc-gen-go v1.36.9` 在服务端仓库根目录执行以下命令。脚本默认将 C# 输出到相邻的 `client` 目录，可通过 `SLG_CLIENT_DIR` 指定其他检出位置。工具路径可用 `PROTOC` 和 `PROTOC_GEN_GO` 覆盖。

```sh
sh scripts/generate-proto.sh
```

## 后续玩法

地图按区块发送带版本的快照和增量。重连时根据客户端已应用版本补发连续增量或重发快照。行军指令用跨重连复用的 `command_id` 去重，服务端回执下发路线、出发时间和预计到达时间，Unity 据此插值。Go 游戏服提交战斗结算与领地变更后再推送结果。
