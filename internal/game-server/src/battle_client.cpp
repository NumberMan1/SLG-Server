#include "battle_client.hpp"

#include <chrono>
#include <iostream>
#include <stdexcept>
#include <thread>
#include <utility>

#include <orbit/tcp_client.hpp>

#include "slg/battle_wire.hpp"
#include "world.hpp"

namespace slg {

BattleClient::BattleClient(Address upstream) : upstream_(std::move(upstream)) {
    if (upstream_.port == 0) throw std::invalid_argument("SLG_BATTLE_RPC_UPSTREAM needs a nonzero port");
}

orbit::rpc::Bytes BattleClient::exchange(std::uint16_t requestId, orbit::rpc::Bytes payload, std::uint16_t replyId) const {
    // 每次建立连接时由 Orbit 解析上游地址，适应战斗服容器重启后的地址变化。
    orbit::TcpClient connection(upstream_.host, upstream_.port, {
        .max_payload = battle::wire::requestSize,
        .timeout = std::chrono::milliseconds(500),
    });
    const orbit::rpc::Frame request{requestId, 1, std::move(payload)};
    connection.send(request);
    auto response = connection.receive();
    if (response.id != replyId || response.sequence != request.sequence) {
        throw std::runtime_error("invalid battle response frame");
    }
    return std::move(response.payload);
}

void BattleClient::monitorReadiness(std::stop_token stop) {
    while (!stop.stop_requested()) {
        auto validUntil = Clock::time_point::min();
        try {
            if (exchange(battle::wire::probeId, {}, battle::wire::probeReplyId).empty()) {
                validUntil = Clock::now() + std::chrono::seconds(2);
            }
        } catch (const std::exception&) {
            // 探测失败时立即标为不可用，下一轮仍会尝试重连。
        }
        readyUntil_.store(validUntil, std::memory_order_relaxed);
        std::this_thread::sleep_for(std::chrono::milliseconds(500));
    }
}

bool BattleClient::ready() const {
    return Clock::now() < readyUntil_.load(std::memory_order_relaxed);
}

void BattleClient::run(std::stop_token stop, World& world) const {
    bool unavailable = false;
    while (!stop.stop_requested()) {
        const auto task = world.pendingBattle();
        if (task) {
            try {
                const auto response = exchange(battle::wire::resolveId, battle::wire::encode(task->request), battle::wire::resultId);
                const auto result = battle::wire::result(response);
                if (!result) throw std::runtime_error("invalid battle result payload");
                world.settleBattle(*task, *result);
                unavailable = false;
            } catch (const std::exception& error) {
                if (!unavailable) std::cerr << "game-server battle retry: " << error.what() << '\n';
                unavailable = true;
                // 失败的请求保留到达时的快照，下次用原种子重试。
                std::this_thread::sleep_for(std::chrono::milliseconds(400));
            }
        }
        std::this_thread::sleep_for(std::chrono::milliseconds(100));
    }
}

}  // slg 命名空间
