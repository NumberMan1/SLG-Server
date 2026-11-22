#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#else
#include <netdb.h>
#include <sys/socket.h>
#endif

#include "battle_client.hpp"

#include <array>
#include <chrono>
#include <exception>
#include <iostream>
#include <memory>
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
    // Orbit C++ 客户端接受数字地址。每次请求解析容器 DNS，允许战斗服重启后更换地址。
    addrinfo hints{};
    hints.ai_family = AF_UNSPEC;
    hints.ai_socktype = SOCK_STREAM;
    addrinfo* raw = nullptr;
    if (getaddrinfo(upstream_.host.c_str(), nullptr, &hints, &raw) != 0) {
        throw std::runtime_error("cannot resolve battle server");
    }
    const std::unique_ptr<addrinfo, decltype(&freeaddrinfo)> addresses(raw, freeaddrinfo);
    const orbit::rpc::Frame request{requestId, 1, std::move(payload)};
    std::exception_ptr failure;
    for (auto* address = addresses.get(); address; address = address->ai_next) {
        std::array<char, NI_MAXHOST> numeric{};
        if (getnameinfo(address->ai_addr, static_cast<socklen_t>(address->ai_addrlen), numeric.data(),
                        static_cast<socklen_t>(numeric.size()), nullptr, 0, NI_NUMERICHOST) != 0) continue;
        try {
            orbit::TcpClient connection(numeric.data(), upstream_.port, {
                .max_payload = battle::wire::requestSize,
                .timeout = std::chrono::milliseconds(500),
            });
            connection.send(request);
            auto response = connection.receive();
            if (response.id != replyId || response.sequence != request.sequence) {
                throw std::runtime_error("invalid battle response frame");
            }
            return std::move(response.payload);
        } catch (...) {
            failure = std::current_exception();
        }
    }
    if (failure) std::rethrow_exception(failure);
    throw std::runtime_error("battle server has no usable address");
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
