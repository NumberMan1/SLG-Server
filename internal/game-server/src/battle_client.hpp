#pragma once

#include <atomic>
#include <chrono>
#include <stop_token>

#include <orbit/rpc.hpp>

#include "address.hpp"
#include "slg/battle.hpp"

namespace slg {

class World;

class BattleClient {
public:
    explicit BattleClient(Address upstream);

    // 等待远端回包期间不持有世界锁。
    void run(std::stop_token stop, World& world) const;
    // 后台探测维护有效期，玩法请求线程只读取结果。
    void monitorReadiness(std::stop_token stop);
    bool ready() const;

private:
    orbit::rpc::Bytes exchange(std::uint16_t requestId, orbit::rpc::Bytes payload, std::uint16_t replyId) const;
    Address upstream_;
    using Clock = std::chrono::steady_clock;
    std::atomic<Clock::time_point> readyUntil_{Clock::time_point::min()};
};

}  // slg 命名空间
