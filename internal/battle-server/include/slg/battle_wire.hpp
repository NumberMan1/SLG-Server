#pragma once

#include "slg/battle.hpp"

#include <cstddef>
#include <span>
#include <vector>

namespace slg::battle::wire {

inline constexpr std::uint16_t resolveId = 1, resultId = 2, probeId = 3, probeReplyId = 4;
inline constexpr std::size_t requestSize = 20;
inline constexpr std::size_t resultSize = 12;
using Bytes = std::vector<std::uint8_t>;

// Orbit 载荷使用固定宽度大端整数，双方共享同一份编解码定义。
template<typename Integer>
void append(Bytes& payload, Integer value) {
    for (std::size_t byte = sizeof(Integer); byte > 0; --byte) {
        payload.push_back(static_cast<std::uint8_t>(value >> ((byte - 1) * 8)));
    }
}

template<typename Integer>
Integer take(std::span<const std::uint8_t>& payload) {
    Integer value = 0;
    for (auto byte : payload.first(sizeof(Integer))) {
        value = (value << 8) | byte;
    }
    payload = payload.subspan(sizeof(Integer));
    return value;
}

inline Bytes encode(const Request& request) {
    Bytes bytes;
    bytes.reserve(requestSize);
    append(bytes, request.rule);
    append(bytes, request.attackerTroops);
    append(bytes, request.defenderTroops);
    append(bytes, request.seed);
    return bytes;
}

inline std::optional<Request> request(std::span<const std::uint8_t> bytes) {
    if (bytes.size() != requestSize) return std::nullopt;
    return Request{take<std::uint32_t>(bytes), take<std::uint32_t>(bytes),
                   take<std::uint32_t>(bytes), take<std::uint64_t>(bytes)};
}

inline Bytes encode(const Result& result) {
    Bytes bytes;
    bytes.reserve(resultSize);
    append(bytes, result.attackerSurvivors);
    append(bytes, result.defenderSurvivors);
    append(bytes, std::uint32_t{result.captured});
    return bytes;
}

inline std::optional<Result> result(std::span<const std::uint8_t> bytes) {
    if (bytes.size() != resultSize) return std::nullopt;
    const auto attacker = take<std::uint32_t>(bytes);
    const auto defender = take<std::uint32_t>(bytes);
    const auto captured = take<std::uint32_t>(bytes);
    if (captured > 1) return std::nullopt;
    return Result{attacker, defender, captured == 1};
}

}  // slg::battle::wire 命名空间
