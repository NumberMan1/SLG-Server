#pragma once

#include <array>
#include <chrono>
#include <cstdint>
#include <deque>
#include <mutex>
#include <optional>
#include <string>
#include <unordered_map>
#include <vector>

#include "json.hpp"
#include "slg/battle.hpp"

namespace slg {

struct BattleTask {
    std::uint64_t id;
    int target, attacker, defender;
    battle::Request request;
};

// World 持有一张地图的权威状态。处理请求和到期结算在同一把锁下完成。
class World {
public:
    explicit World(std::chrono::seconds seatIdleTimeout);

    // 查询会为首次出现的账号分配出生地，行军请求只允许已进入地图的账号。
    nlohmann::json update(const std::string& account, const nlohmann::json& request);

    // 只由战斗工作线程读取。失败时重复返回同一份部队快照和种子。
    std::optional<BattleTask> pendingBattle();
    // 回包时再次核对行军和领地，防止过期请求改变当前世界。
    void settleBattle(const BattleTask& task, const battle::Result& result);

private:
    using Clock = std::chrono::steady_clock;

    // 与 proto/world.proto 中的 WorldResult 数值一致。
    enum class Result {
        ok = 0, full = 1, invalid_target = 2, already_marching = 3,
        reserved = 4, invalid_order = 5, world_changed = 6, insufficient_troops = 8
    };

    struct Tile {
        int id, q, r;
        int home = 0;
        int owner = 0;
        std::uint32_t garrison = 0;
    };

    struct March {
        int source, target;
        std::uint32_t troops;
        Clock::time_point arrives;
        std::optional<BattleTask> battle;
    };

    struct PlacedOrder {
        int target;
        std::uint32_t troops;
    };

    struct Player {
        std::string account;
        Clock::time_point lastSeen;
        Clock::time_point lastReinforcement;
        std::uint32_t reserves = battle::maximumTroops;
        std::uint64_t reserveRevision = 0;
        std::unordered_map<std::string, PlacedOrder> orders;
        std::optional<March> marching;
    };

    int findPlayer(const std::string& account) const;
    int join(const std::string& account, Clock::time_point now);
    Result placeOrder(int index, const nlohmann::json& order, Clock::time_point now);
    void expireSeats(Clock::time_point now);
    void advance(Clock::time_point now);
    nlohmann::json snapshot(int index, std::uint64_t revision, Clock::time_point now, std::int64_t unixMs) const;

    std::mutex mutex_;
    std::string instance_;
    const std::chrono::seconds seatIdleTimeout_;
    std::uint64_t revision_ = 1;
    std::uint64_t nextRevision_ = 2;
    std::vector<Tile> tiles_;
    std::array<Player, 2> players_;
    std::uint64_t nextBattleId_ = 1;
    std::deque<nlohmann::json> battleReports_;
};

}  // slg 命名空间
