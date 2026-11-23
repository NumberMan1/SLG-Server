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
    const auto limit = request.rule == currentRule ? maximumTroops : maximumBattleTroops;
    if ((request.rule != currentRule && request.rule != exchangeRule && request.rule != decisiveRule &&
         request.rule != continuousRule) ||
        attacker == 0 || attacker > limit || defender == 0 || defender > limit) {
        return std::nullopt;
    }
    if (request.rule == exchangeRule) {
        // 前两回合消耗兵力，双方至少保留一兵，地块归属在决胜回合确定。
        return Result{attacker - std::min(attacker - 1, (defender + 3) / 4),
                      defender - std::min(defender - 1, (attacker + 3) / 4), false};
    }
    auto seed = request.seed;
    if (request.rule == continuousRule) {
        // 以本次接战兵力同时计算双方损失。双方同时耗尽时守军保留一兵。
        const auto attackDamage = std::max(1u, attacker * (90 + roll(seed)) / 800);
        const auto defenseDamage = std::max(1u, defender * (100 + roll(seed)) / 800);
        const auto attacking = attacker - std::min(attacker, defenseDamage);
        auto defending = defender - std::min(defender, attackDamage);
        if (attacking == 0 && defending == 0) defending = 1;
        return Result{attacking, defending, defending == 0};
    }
    const auto attackPower = attacker * (90 + roll(seed));
    const auto defensePower = defender * (100 + roll(seed));
    // 一次性结算和决胜回合都由守军赢得平局，获胜方至少保留一名士兵。
    if (attackPower > defensePower) {
        return Result{attacker - std::min(attacker - 1, (defender + 1) / 2), 0, true};
    }
    return Result{attacker - std::min(attacker, (defender + 1) / 2),
                  defender - std::min(defender - 1, (attacker + 1) / 2), false};
}

}  // slg::battle 命名空间
