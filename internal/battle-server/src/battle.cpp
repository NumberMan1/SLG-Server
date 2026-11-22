#include "slg/battle.hpp"

#include <algorithm>

namespace slg::battle {
namespace {

std::uint32_t roll(std::uint64_t& seed) {
    // SplitMix64 使用固定整数运算，避免随机分布随标准库实现变化。
    seed += 0x9e3779b97f4a7c15ULL;
    auto value = seed;
    value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9ULL;
    value = (value ^ (value >> 27)) * 0x94d049bb133111ebULL;
    return static_cast<std::uint32_t>((value ^ (value >> 31)) % 21);
}

}  // 匿名命名空间

std::optional<Result> resolve(const Request& request) {
    const auto attacker = request.attackerTroops;
    const auto defender = request.defenderTroops;
    if (request.rule != currentRule || attacker == 0 || attacker > maximumTroops ||
        defender == 0 || defender > maximumTroops) {
        return std::nullopt;
    }
    auto seed = request.seed;
    const auto attackPower = attacker * (90 + roll(seed));
    const auto defensePower = defender * (100 + roll(seed));
    // 平局由守军守住地块，获胜方至少保留一名士兵。
    if (attackPower > defensePower) {
        return Result{attacker - std::min(attacker - 1, (defender + 1) / 2), 0, true};
    }
    return Result{attacker - std::min(attacker, (defender + 1) / 2),
                  defender - std::min(defender - 1, (attacker + 1) / 2), false};
}

}  // slg::battle 命名空间
