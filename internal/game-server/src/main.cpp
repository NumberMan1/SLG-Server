#include <charconv>
#include <cstdlib>
#include <iostream>
#include <stdexcept>
#include <string>
#include <string_view>
#include <thread>

#include <orbit/request_server.hpp>

#include "httplib.h"
#include "battle_client.hpp"
#include "world.hpp"

namespace {

std::chrono::seconds seatIdleTimeout() {
    const auto configured = std::getenv("SLG_WORLD_IDLE_SECONDS");
    if (!configured) {
        return std::chrono::seconds(60);
    }
    const std::string_view text(configured);
    const char* end = text.data() + text.size();
    int seconds = 0;
    const auto [next, error] = std::from_chars(text.data(), end, seconds);
    if (error != std::errc{} || next != end || seconds <= 0) {
        throw std::invalid_argument("SLG_WORLD_IDLE_SECONDS must be a positive integer");
    }
    return std::chrono::seconds(seconds);
}

// 内网消息 ID 与 proto/world.proto 中的 WorldMessageId 一致。
constexpr std::uint16_t worldRequestId = 3;
constexpr std::uint16_t worldReplyId = 4;
// 仅网关内部使用的探活帧，独立于玩法消息，不会占用世界名额。
constexpr std::uint16_t worldProbeId = 5;
constexpr std::uint16_t worldProbeReplyId = 6;
constexpr std::size_t maxWorldPayload = 4096;
// 网关在一条连接上复用各玩家的请求，接收上限与网关请求匹配器一致。
constexpr std::size_t maxWorldPending = 8;

orbit::RequestReply reply(std::string payload) {
    return {worldReplyId, {payload.begin(), payload.end()}};
}

orbit::RequestReply handleWorld(slg::World& world, const orbit::rpc::Frame& frame) {
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
    // 账号由可信内网的网关从已认证会话填写，世界状态仍由游戏服持有。
    return reply(world.update(account, body["request"]).dump());
}

}  // 匿名命名空间

int main() {
    try {
        const auto http = slg::addressFromEnv("SLG_GAME_ADDR", "127.0.0.1:8081");
        const auto rpc = slg::addressFromEnv("SLG_GAME_RPC_ADDR", "127.0.0.1:8083");
        slg::BattleClient battle(slg::addressFromEnv("SLG_BATTLE_RPC_UPSTREAM", "127.0.0.1:8084"));
        slg::World world(seatIdleTimeout());
        orbit::RequestServer requests(rpc.host, rpc.port, {
            {worldRequestId, [&](std::stop_token, const orbit::rpc::Frame& frame) {
                return handleWorld(world, frame);
            }},
            {worldProbeId, [&](std::stop_token, const orbit::rpc::Frame&) {
                return orbit::RequestReply{worldProbeReplyId, battle.ready() ? orbit::rpc::Bytes{} : orbit::rpc::Bytes{1}};
            }},
        }, {
            .max_payload = maxWorldPayload,
            .max_concurrent = maxWorldPending,
            .max_pending_per_connection = maxWorldPending,
        });
        std::jthread battleWorker([&](std::stop_token stop) { battle.run(stop, world); });
        std::jthread battleMonitor([&](std::stop_token stop) { battle.monitorReadiness(stop); });

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
        // 两个监听器绑定后才报告就绪；测试用端口 0 让内核选取可用端口。
        std::cout << "game-server rpc listening " << rpc.host << ':' << requests.port() << std::endl;
        std::cout << "game-server http listening " << http.host << ':' << port << std::endl;
        return health.listen_after_bind() ? 0 : 1;
    } catch (const std::exception& error) {
        std::cerr << "game-server: " << error.what() << '\n';
        return 1;
    }
}
