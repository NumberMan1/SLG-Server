#pragma once

#include <charconv>
#include <cstdint>
#include <cstdlib>
#include <stdexcept>
#include <string>

namespace slg {

struct Address {
    std::string host;
    std::uint16_t port;
};

// 监听端口 0 用于测试，由系统分配可用端口。
inline Address addressFromEnv(const char* name, const char* fallback) {
    const auto configured = std::getenv(name);
    const std::string address = configured ? configured : fallback;
    const auto split = address.rfind(':');
    if (split == std::string::npos || split == 0) {
        throw std::invalid_argument(std::string(name) + " must use host:port");
    }
    auto host = address.substr(0, split);
    if (host.front() == '[' && host.back() == ']') {
        host = host.substr(1, host.size() - 2);
    }
    const auto end = address.data() + address.size();
    std::uint16_t port = 0;
    const auto [next, error] = std::from_chars(address.data() + split + 1, end, port);
    if (error != std::errc{} || next != end || host.empty()) {
        throw std::invalid_argument(std::string(name) + " contains an invalid address");
    }
    return {host, port};
}

}  // slg 命名空间
