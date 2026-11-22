#pragma once

#include <cstdlib>
#include <stdexcept>
#include <string>

#include <orbit/tcp_endpoint.hpp>

namespace slg {

using Address = orbit::TcpEndpoint;

// 监听端口 0 用于测试，由系统分配可用端口。
inline Address addressFromEnv(const char* name, const char* fallback) {
    const auto configured = std::getenv(name);
    try {
        return orbit::parse_tcp_endpoint(configured ? configured : fallback, orbit::TcpEndpointUse::listen);
    } catch (const std::invalid_argument& error) {
        throw std::invalid_argument(std::string(name) + ": " + error.what());
    }
}

}  // slg 命名空间
