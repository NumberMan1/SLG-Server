#pragma once

#include <cstdint>
#include <optional>

namespace slg::battle {

inline constexpr std::uint32_t currentRule = 1;
inline constexpr std::uint32_t exchangeRule = 2;
inline constexpr std::uint32_t decisiveRule = 3;
inline constexpr std::uint32_t continuousRule = 4;
inline constexpr std::uint32_t maximumTroops = 100;
inline constexpr std::uint32_t maximumBattleTroops = 300;

struct Request {
    std::uint32_t rule;
    std::uint32_t attackerTroops;
    std::uint32_t defenderTroops;
    std::uint64_t seed;
};

struct Result {
    std::uint32_t attackerSurvivors;
    std::uint32_t defenderSurvivors;
    bool captured;
};

// 按冻结的部队快照计算战损。相同规则版本、兵力和随机种子产生相同结果。
std::optional<Result> resolve(const Request& request);

}  // slg::battle 命名空间
