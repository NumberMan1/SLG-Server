#pragma once

#include <array>
#include <chrono>
#include <cstdint>
#include <deque>
#include <map>
#include <optional>
#include <string>
#include <unordered_map>
#include <vector>

#include "json.hpp"
#include "slg/battle.hpp"

namespace slg {

// World 持有一张地图的权威状态，只能由 WorldActor 的队列线程调用。
class World {
public:
    explicit World(std::chrono::seconds seatIdleTimeout);

    // 查询会为首次出现的账号分配出生地，行军请求只允许已进入地图的账号。
    nlohmann::json update(const std::string& account, const nlohmann::json& request);
    // 周期推进无人查询的地图，和请求处理共用同一套到期规则。
    void tick();

private:
    using Clock = std::chrono::steady_clock;

    // 与 proto/world.proto 中的 WorldResult 数值一致。
    enum class Result {
        ok = 0, full = 1, invalid_target = 2, already_marching = 3,
        reserved = 4, invalid_order = 5, world_changed = 6, insufficient_troops = 8,
        reinforcement_closed = 9, battle_capacity = 10, battle_closed = 11
    };

    enum class CombatMode { instant, rounds, continuous };

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
        std::uint64_t dispatchRevision;
        CombatMode combatMode;
        std::uint64_t reinforcement;
    };

    struct PlacedOrder {
        int target;
        std::uint32_t troops;
        CombatMode combatMode;
        std::uint64_t reinforcement;
        std::uint64_t retreat;

        bool operator==(const PlacedOrder&) const = default;
    };

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

        bool continuous() const { return rounds == 0; }
        bool acceptsReinforcement() const { return continuous() || task.round < rounds; }
    };
    using Engagements = std::map<std::uint64_t, Engagement>;

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
    Result retreat(int index, const std::string& commandId, const PlacedOrder& order);
    void expireSeats(Clock::time_point now);
    void advance(Clock::time_point now);
    void step(Clock::time_point now);
    void applyBattleResult(Engagements::iterator current, const battle::Result& result, Clock::time_point now);
    void finishBattle(const Engagement& engagement, const battle::Result& result, int retreatedPlayer = 0);
    nlohmann::json snapshot(int index, std::uint64_t revision, Clock::time_point now, std::int64_t unixMs) const;

    std::string instance_;
    const std::chrono::seconds seatIdleTimeout_;
    std::uint64_t revision_ = 1;
    std::uint64_t nextRevision_ = 2;
    std::vector<Tile> tiles_;
    std::array<Player, 2> players_;
    std::uint64_t nextBattleId_ = 1;
    Engagements engagements_;
    std::uint64_t nextReportId_ = 1;
    std::deque<nlohmann::json> battleReports_;
};

}  // slg 命名空间
