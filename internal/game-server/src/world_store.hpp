#pragma once

#include <array>
#include <cstdint>
#include <chrono>
#include <functional>
#include <memory>
#include <optional>
#include <string>
#include <string_view>

#include "world_checkpoint.hpp"

namespace slg {

class World;

// 事件按世界序号提交，检查点可由独立后台线程安装并清理已覆盖的事件。
class WorldStore {
public:
    struct Snapshot {
        std::uint64_t revision;
        std::string payload;
        std::optional<bool> checksumMatches;
    };
    struct SavedWorld {
        std::uint64_t checkpoint;
        std::optional<std::uint64_t> head;
        std::uint64_t pruneThrough;
        bool recoveredFromPrevious;
        std::chrono::steady_clock::time_point at;
        std::vector<SavedOrder> legacyOrders;
    };

    // 无数据库配置时允许直接运行临时试玩；指定持久世界 ID 时要求数据库配置。
    static std::unique_ptr<WorldStore> fromEnvironment();

    static constexpr auto leaseDuration = std::chrono::seconds(2);
    bool acquireLease(const std::string& writer) const;
    bool renewLease(const std::string& writer) const;
    std::optional<SavedWorld> load(World& world) const;
    // 从已有快照版本或空库建立事件流，旧版内嵌指令也在同一事务迁入。
    bool initialize(std::optional<std::uint64_t> previous, const std::string& snapshot, const std::vector<SavedOrder>& orders) const;
    // 返回 false 表示其他写者已推进此世界的版本。连接错误可沿用原参数重试。
    bool append(std::uint64_t expectedRevision, const WorldEvent& event, const std::string& writer) const;
    // 安装新快照，返回由上一份可用快照覆盖的序号。修复快照时保留原回退点。
    std::uint64_t checkpoint(std::uint64_t revision, const std::string& snapshot, bool repairing) const;
    // 每次最多清理一批。返回 false 时还有可能需要下一批。
    bool pruneEvents(std::uint64_t coveredThrough) const;
    bool pruneOrders(std::uint64_t before, const std::array<std::uint64_t, 2>& active) const;

private:
    struct Config {
        std::string host, user, password, schema;
        unsigned port;
        bool tls;
        unsigned connectTimeout = 2, readTimeout = 2, writeTimeout = 2;
    };
    class Connection;

    WorldStore(Config config, std::string world);
    Connection& maintenanceConnection() const;
    Connection& cleanupConnection() const;
    std::optional<Snapshot> loadSnapshot(Connection& connection, bool previous = false) const;
    void saveSnapshotChecksum(Connection& connection, std::uint64_t revision) const;
    std::optional<std::uint64_t> loadVerifiedRevision(Connection& connection, bool previous) const;
    std::optional<std::uint64_t> loadRevision(Connection& connection, std::string_view table, bool lock = false) const;
    void loadEvents(Connection& connection, std::uint64_t after, std::uint64_t head,
                    const std::function<void(std::string_view)>& apply) const;
    void loadOrders(Connection& connection, World& world) const;
    void insertOrders(Connection& connection, const std::vector<SavedOrder>& orders) const;
    bool lockLease(Connection& connection, const std::string& writer) const;

    Config config_;
    const std::string world_;
    const std::string key_;
};

}  // slg 命名空间
