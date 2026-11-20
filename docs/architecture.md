# 服务端架构

## 服务边界

- **Go 运行时**由 Orbit 的 `service.App` 管理启动和优雅关闭，`socket/http` 提供 HTTP 服务、客户端和 JSON。Numbox 的 `utils/env` 读取启动配置。后续行军到期调度可在 Orbit 串行队列中推进 Numbox 时间轮。
- **Go 网关**负责客户端入口，将 HTTP 登录请求转发给登录服务，在 Orbit TCP 握手时消费 Redis 票据，并转发 `GET /v1/game/health`。
- **Go 登录服务**负责管理账号表、校验 MySQL 中的密码哈希，并将 30 秒一次性票据写入 Redis。它通过 etcd 公布内网地址。
- **Go 游戏服**负责地图、行军、占领和联盟状态。建议先按世界分区组织状态归属，由游戏服处理同一地块的并发占领与重复结算。
- **C++ 战斗服**负责战斗计算。首个版本可采用无状态结算，输入部队快照、规则版本与随机种子，输出战果和战报。先通过 CTest 验证固定输入的可重放性，再接入跨语言 RPC。接口稳定后把协议源集中放到 `server/proto`。
- **Unity 客户端**负责地图展示、指令提交与行军表现。服务端状态决定部队到达、战果及领地归属。

## 数据与组件

SLG 的数据库选型取决于状态模型和读写路径。这个项目优先选 **MySQL 8.4 / InnoDB**。地块归属、联盟成员、行军状态和结算流水适合用关系表、唯一约束与事务表达。例如，先用唯一的结算 ID 去重，再在同一事务内写入结算流水和领地变更。首个 SQL 迁移创建登录账号表。[数据库目录](../database/README.md) · [MySQL InnoDB 文档](https://dev.mysql.com/doc/refman/8.4/en/innodb-introduction.html)

MongoDB 适合按单个文档整体读写的聚合，例如结构经常演进的独立战报。它也支持多文档事务，建模时应考虑文档边界与跨文档事务成本。首版战报可先用 MySQL 的 JSON 字段保存详情，再根据查询量和访问模式评估是否引入 MongoDB。[MongoDB 原子性与事务](https://www.mongodb.com/docs/manual/core/write-operations-atomicity/)

| 组件 | 用途 | 状态 |
| --- | --- | --- |
| MySQL | 持久化账号、世界状态、玩家、联盟和结算 | 已接入账号，连接池由 Orbit `storage/mysql` 创建 |
| Redis | 短期登录票据、可重建的热点地块缓存和排行榜 | 已接入登录票据，通过 Orbit `storage/redis` 支持 Cluster 和本地单机 |
| Kafka | 战斗结算后的异步事件，例如战报分发和统计 | 存在独立消费者时，与事务 outbox 和消费者去重一起实现 |
| etcd | 多实例服务注册、发现与租约 | 登录服务和游戏服监听后注册，网关通过 Orbit `discovery.Store` 订阅实例变化 |

网关轮询已发现的登录服务实例，每次登录请求只发送一次。游戏服健康查询在请求时限内可以尝试其他实例，没有可用实例时返回 `502`。服务正常停止时撤销租约，异常退出后 etcd 按租约过期移除实例。Redis 的 `GETDEL` 在槽位主节点上原子消费票据。Cluster 异步复制时，主节点故障切换可能丢失最近的写入，包括消费造成的删除。
