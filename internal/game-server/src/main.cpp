#include <charconv>
#include <cstdlib>
#include <iostream>
#include <memory>
#include <semaphore>
#include <stdexcept>
#include <string>
#include <string_view>
#include <thread>

#include <orbit/request_server.hpp>

#include "address.hpp"
#include "httplib.h"
#include "world_actor.hpp"

namespace {

int positiveFromEnv(const char* key, int fallback) {
    const auto configured = std::getenv(key);
    if (!configured) {
        return fallback;
    }
    const std::string_view text(configured);
    const char* end = text.data() + text.size();
    int seconds = 0;
    const auto [next, error] = std::from_chars(text.data(), end, seconds);
    if (error != std::errc{} || next != end || seconds <= 0) {
        throw std::invalid_argument(std::string(key) + " must be a positive integer");
    }
    return seconds;
}

// 内网消息 ID 与 proto/world.proto 中的 WorldMessageId 一致。
constexpr std::uint16_t worldRequestId = 3;
constexpr std::uint16_t worldReplyId = 4;
// 仅网关内部使用的探活帧，独立于玩法消息，不会占用世界名额。
constexpr std::uint16_t worldProbeId = 5;
constexpr std::uint16_t worldProbeReplyId = 6;
constexpr std::size_t maxWorldPayload = 32768;
// 网关在一条连接上复用各玩家的请求，接收上限与网关请求匹配器一致。
constexpr std::size_t maxWorldPending = 8;
using MutationSlots = std::counting_semaphore<maxWorldPending / 2>;

orbit::RequestReply reply(std::string payload) {
    return {worldReplyId, {payload.begin(), payload.end()}};
}

orbit::RequestReply handleWorld(slg::WorldActor& world, MutationSlots& slots, const orbit::rpc::Frame& frame) {
    constexpr auto unavailable = R"({"result":7})";
    const auto body = nlohmann::json::parse(frame.payload.begin(), frame.payload.end(), nullptr, false);
    if (!body.is_object() || !body.contains("account") || !body["account"].is_string() ||
        !body.contains("request") || !body["request"].is_object()) {
        return reply(unavailable);
    }
    const auto& account = body["account"].get_ref<const std::string&>();
    if (account.empty() || account.size() > 64) {
        return reply(unavailable);
    }
    const auto& request = body["request"];
    const bool writes = request.contains("order");
    if (writes && !slots.try_acquire()) return reply(unavailable);
    // 等待 MySQL 的在途指令只占用一部分 RPC 槽位，余下的用于查询和探活。
    const auto release = [](MutationSlots* slot) { slot->release(); };
    const std::unique_ptr<MutationSlots, decltype(release)> slot(writes ? &slots : nullptr, release);
    const auto result = world.update(account, request);
    return reply(result ? result->dump() : unavailable);
}

}  // 匿名命名空间

int main() {
    try {
        const auto http = slg::addressFromEnv("SLG_GAME_ADDR", "127.0.0.1:8081");
        const auto rpc = slg::addressFromEnv("SLG_GAME_RPC_ADDR", "127.0.0.1:8083");
        slg::WorldActor world(std::chrono::seconds(positiveFromEnv("SLG_WORLD_IDLE_SECONDS", 60)),
            std::chrono::seconds(positiveFromEnv("SLG_WORLD_CHECKPOINT_SECONDS", 30)),
            positiveFromEnv("SLG_WORLD_RADIUS", 2), slg::WorldStore::fromEnvironment());
        MutationSlots mutationSlots(maxWorldPending / 2);
        orbit::RequestServer requests(rpc.host, rpc.port, {
            {worldRequestId, [&](std::stop_token, const orbit::rpc::Frame& frame) {
                return handleWorld(world, mutationSlots, frame);
            }},
            {worldProbeId, [&](std::stop_token, const orbit::rpc::Frame&) {
                return orbit::RequestReply{worldProbeReplyId, world.ready() ? std::vector<std::uint8_t>{} : std::vector<std::uint8_t>{1}};
            }},
        }, {
            .max_payload = maxWorldPayload,
            .max_concurrent = maxWorldPending,
            .max_pending_per_connection = maxWorldPending,
        });
        httplib::Server health;
        health.new_task_queue = [] { return new httplib::ThreadPool(4); };
        health.set_read_timeout(5, 0);
        health.set_write_timeout(5, 0);
        health.Get("/healthz", [&](const httplib::Request&, httplib::Response& response) {
            if (requests.error()) {
                response.status = 503;
                return;
            }
            response.set_content(R"({"service":"game-server","status":"ok"})", "application/json");
        });

        int port = http.port;
        if (port == 0) {
            port = health.bind_to_any_port(http.host);
        } else if (!health.bind_to_port(http.host, port)) {
            port = -1;
        }
        if (port < 0) {
            throw std::runtime_error("game-server failed to bind HTTP listener");
        }
        std::jthread leaseMonitor([&](std::stop_token stop) {
            while (!stop.stop_requested()) {
                if (world.leaseLost()) {
                    std::cerr << "world lease lost; restarting requires reloading the durable world\n";
                    // 避开可能仍阻塞在数据库的线程析构，未完成的事务由数据库回滚。
                    std::_Exit(EXIT_FAILURE);
                }
                std::this_thread::sleep_for(std::chrono::milliseconds(50));
            }
        });
        // 两个监听器绑定后才报告就绪；测试用端口 0 让内核选取可用端口。
        std::cout << "game-server rpc listening " << rpc.host << ':' << requests.port() << std::endl;
        std::cout << "game-server http listening " << http.host << ':' << port << std::endl;
        return health.listen_after_bind() ? 0 : 1;
    } catch (const std::exception& error) {
        std::cerr << "game-server: " << error.what() << '\n';
        return 1;
    }
}
