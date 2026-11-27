#pragma once

#include <array>
#include <chrono>
#include <cstdint>
#include <deque>
#include <functional>
#include <map>
#include <memory>
#include <optional>
#include <set>
#include <stdexcept>
#include <string>
#include <string_view>
#include <unordered_map>
#include <vector>

#include "json.hpp"
#include "slg/battle.hpp"
#include "world_checkpoint.hpp"

namespace slg {

// 地图配置不兼容时停止启动，避免按较小的回退快照丢弃扩展地块。
class WorldLayoutError : public std::runtime_error {
public:
    using std::runtime_error::runtime_error;
};

// World 持有一张地图的权威状态，只能由 WorldActor 的队列线程调用。
class World {
public:
    using Clock = std::chrono::steady_clock;
    using ApplyEvent = std::function<void(std::string_view)>;
    struct RestoreState {
        Clock::time_point at;
        bool legacyOrders;
    };
    // 与 proto/world.proto 中的 WorldResult 数值一致。
    enum class Result {
        ok = 0, full = 1, invalid_target = 2, already_marching = 3,
        reserved = 4, invalid_order = 5, world_changed = 6, insufficient_troops = 8,
        reinforcement_closed = 9, battle_capacity = 10, battle_closed = 11
    };

    explicit World(std::chrono::seconds seatIdleTimeout, int radius = 2);

    // 查询会为首次出现的账号分配出生地，行军请求只允许已进入地图的账号。
    Result update(const std::string& account, const nlohmann::json& request);
    // 周期推进无人查询的地图，和请求处理共用同一套到期规则。
    void tick();
    // 写入进行中可读取已提交状态，已有玩家的访问仍刷新离线计时。
    nlohmann::json read(const std::string& account, const nlohmann::json& request, std::optional<Result> result = {}) const;
    bool hasPlayer(const std::string& account) const;
    bool hasOpenSeat() const;
    void touch(const std::string& account);
    std::uint64_t stateVersion() const { return nextRevision_; }
    const std::string& instance() const { return instance_; }
    bool recordReplenishmentSince(Clock::time_point saved, Clock::time_point now);

    // 检查点和事件共用单调的世界时间，回放只应用已计算的变更。
    std::string save(Clock::time_point at) const;
    std::string event(const World& previous, Clock::time_point at, const std::string& writer) const;
    void publish(World& previous);
    void clearChanges();
    // 顺序回放日志，返回恢复时钟的基准和是否需要迁移旧版内嵌指令。
    RestoreState restore(std::string_view payload, const std::function<void(const ApplyEvent&)>& readEvents, bool journaled);
    // 读取大量指令回执的等待时间也计入停机，开放游戏前再恢复推进。
    void pauseFor(Clock::duration duration);
    void restoreOrder(SavedOrder order);
    std::vector<SavedOrder> legacyOrders() const;
    std::optional<SavedOrder> takeOrder();
    std::array<std::uint64_t, 2> seats() const;

private:
    static constexpr std::size_t battleReportLimit = 8;
    enum class CombatMode { instant, rounds, continuous };

    struct Tile {
        int id, q, r;
        int home = 0;
        int owner = 0;
        std::uint32_t garrison = 0;
        battle::TroopType garrisonType = battle::TroopType::militia;
        battle::Terrain terrain() const;
    };

    struct March {
        int source, target;
        std::uint32_t troops;
        Clock::time_point arrives;
        std::uint64_t dispatchRevision;
        CombatMode combatMode;
        std::uint64_t reinforcement;
        battle::TroopType troopType = battle::TroopType::militia;
        bool operator==(const March&) const = default;
    };

    struct PlacedOrder {
        int target;
        std::uint32_t troops;
        CombatMode combatMode;
        std::uint64_t reinforcement;
        std::uint64_t retreat;
        battle::TroopType troopType = battle::TroopType::militia;

        bool operator==(const PlacedOrder&) const = default;
    };
    using OrderHistory = std::unordered_map<std::string, PlacedOrder>;

    struct BattleTask {
        std::uint64_t id;
        int target, attacker, defender;
        battle::Request request;
        unsigned int round;
    };

    struct Engagement {
        BattleTask task;
        unsigned int rounds;
        Clock::time_point roundEnds;
        std::uint64_t seed;
        std::uint32_t initialAttacker, initialDefender;
        std::uint32_t attackerWaiting = 0, defenderWaiting = 0;
        std::uint32_t attackerReinforcements = 0, defenderReinforcements = 0;
        std::uint32_t lastAttackerLosses = 0, lastDefenderLosses = 0;

        bool continuous() const { return rounds == 0; }
        bool acceptsReinforcement() const { return continuous() || task.round < rounds; }
    };
    using Engagements = std::map<std::uint64_t, Engagement>;

    struct Player {
        std::string account;
        Clock::time_point lastSeen;
        Clock::time_point lastReinforcement;
        std::uint32_t reserves = battle::maximumTroops;
        std::uint64_t seat = 0;
        // 已发布的读视图只读取地图字段，与权威世界共享但不访问指令记录。
        std::shared_ptr<OrderHistory> orders;
        std::optional<March> marching;
    };

    int findPlayer(const std::string& account) const;
    int join(const std::string& account, Clock::time_point now);
    Result placeOrder(int index, const nlohmann::json& order, Clock::time_point now, bool sameInstance);
    Result retreat(int index, const std::string& commandId, const PlacedOrder& order, Clock::time_point now);
    static std::uint32_t reservesAt(const Player& player, Clock::time_point now);
    static void settleReserves(Player& player, Clock::time_point now);
    static void returnTroops(Player& player, std::uint32_t troops, Clock::time_point now);
    void rememberOrder(Player& player, const std::string& id, const PlacedOrder& order);
    void setTileState(Tile& tile, int owner, std::uint32_t garrison, battle::TroopType type = battle::TroopType::militia);
    static SavedOrder savedOrder(std::uint64_t seat, const std::string& id, const PlacedOrder& order);
    void expireSeats(Clock::time_point now);
    void advance(Clock::time_point now);
    void step(Clock::time_point now);
    void settleDueBattles(Clock::time_point now);
    void applyBattleResult(Engagements::iterator current, const battle::Result& result, Clock::time_point now);
    void finishBattle(const Engagement& engagement, const battle::Result& result, Clock::time_point now, int retreatedPlayer = 0);
    nlohmann::json reply(int index, Result result, const nlohmann::json& request, Clock::time_point now) const;
    nlohmann::json snapshot(int index, std::uint64_t revision, std::uint32_t reserves, Clock::time_point now,
                            std::int64_t unixMs, bool includeTiles) const;
    std::int64_t logicalTime(Clock::time_point time) const;
    nlohmann::json persistedHeader(Clock::time_point at) const;
    nlohmann::json persistedPlayer(Player player, Clock::time_point at) const;
    nlohmann::json persistedBattle(const Engagement& battle) const;

    std::string instance_;
    std::uint64_t syncRevisionOffset_;
    Clock::time_point logicalEpoch_ = Clock::now();
    std::chrono::seconds seatIdleTimeout_;
    int radius_;
    std::uint64_t revision_ = 1;
    std::uint64_t tileRevision_ = 1;
    std::uint64_t nextRevision_ = 2;
    std::vector<Tile> tiles_;
    std::array<Player, 2> players_;
    std::uint64_t nextBattleId_ = 1;
    Engagements engagements_;
    std::uint64_t nextReportId_ = 1;
    std::deque<nlohmann::json> battleReports_;
    std::optional<SavedOrder> newOrder_;
    std::set<int> changedTiles_;
    std::set<std::uint64_t> changedBattles_;
};

}  // slg 命名空间
