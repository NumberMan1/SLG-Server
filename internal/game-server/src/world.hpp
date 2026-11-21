#pragma once

#include <array>
#include <chrono>
#include <cstdint>
#include <mutex>
#include <optional>
#include <string>
#include <unordered_map>
#include <vector>

#include "json.hpp"

namespace slg {

// World 持有一张地图的权威状态。处理请求和到期结算在同一把锁下完成。
class World {
public:
    explicit World(std::chrono::seconds seatIdleTimeout);

    // 查询会为首次出现的账号分配出生地，行军请求只允许已进入地图的账号。
    nlohmann::json update(const std::string& account, const nlohmann::json& request);

private:
    using Clock = std::chrono::steady_clock;

    // 与 proto/world.proto 中的 WorldResult 数值一致。
    enum class Result {
        ok = 0, full = 1, invalid_target = 2, already_marching = 3,
        reserved = 4, invalid_order = 5, world_changed = 6
    };

    struct Tile {
        int id, q, r;
        int home = 0;
        int owner = 0;
    };

    struct March {
        int source, target;
        Clock::time_point arrives;
    };

    struct Player {
        std::string account;
        Clock::time_point lastSeen;
        std::unordered_map<std::string, int> orders;
        std::optional<March> marching;
    };

    int findPlayer(const std::string& account) const;
    int join(const std::string& account);
    Result placeOrder(int index, const nlohmann::json& order, Clock::time_point now);
    void expireSeats(Clock::time_point now);
    void advance(Clock::time_point now);
    nlohmann::json snapshot(int index, Clock::time_point now, std::int64_t unixMs) const;

    std::mutex mutex_;
    std::string instance_;
    const std::chrono::seconds seatIdleTimeout_;
    std::uint64_t revision_ = 1;
    std::vector<Tile> tiles_;
    std::array<Player, 2> players_;
};

}  // slg 命名空间
