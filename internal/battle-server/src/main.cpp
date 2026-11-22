#include <csignal>
#include <iostream>
#include <string_view>
#include <thread>

#include <orbit/request_server.hpp>

#include "address.hpp"
#include "slg/battle_wire.hpp"

namespace {

volatile std::sig_atomic_t stopping = 0;

void stop(int) { stopping = 1; }

orbit::RequestReply settle(std::stop_token, const orbit::rpc::Frame& frame) {
    const auto request = slg::battle::wire::request(frame.payload);
    const auto result = request ? slg::battle::resolve(*request) : std::nullopt;
    return {slg::battle::wire::resultId, result ? slg::battle::wire::encode(*result) : slg::battle::wire::Bytes{}};
}

}  // 匿名命名空间

int main(int argc, char* argv[]) {
    // CTest 通过退出码验证战斗服可执行文件能启动并处理健康检查参数。
    if (argc == 2 && std::string_view(argv[1]) == "--healthcheck") {
        std::cout << "battle-server: ok\n";
        return 0;
    }

    if (argc != 1) {
        std::cerr << "Usage: battle-server [--healthcheck]\n";
        return 2;
    }
    try {
        const auto address = slg::addressFromEnv("SLG_BATTLE_RPC_ADDR", "127.0.0.1:8084");
        std::signal(SIGINT, stop);
        std::signal(SIGTERM, stop);
        orbit::RequestServer requests(address.host, address.port, {
            {slg::battle::wire::resolveId, settle},
            {slg::battle::wire::probeId, [](std::stop_token, const orbit::rpc::Frame&) {
                return orbit::RequestReply{slg::battle::wire::probeReplyId, {}};
            }},
        }, {.max_payload = slg::battle::wire::requestSize});
        std::cout << "battle-server rpc listening " << address.host << ':' << requests.port() << std::endl;
        while (!stopping) {
            if (const auto error = requests.error()) std::rethrow_exception(error);
            std::this_thread::sleep_for(std::chrono::milliseconds(100));
        }
    } catch (const std::exception& error) {
        std::cerr << "battle-server: " << error.what() << '\n';
        return 1;
    }
    return 0;
}
