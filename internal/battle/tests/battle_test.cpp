#include "slg/battle.hpp"

#include <iostream>

int main() {
    using namespace slg::battle;
    const Request attack{currentRule, 60, 30, 123};
    const auto original = resolve(attack);
    const auto replay = resolve(attack);
    if (!original || !replay || !original->captured || original->attackerSurvivors != 45 ||
        original->attackerSurvivors != replay->attackerSurvivors || original->defenderSurvivors != replay->defenderSurvivors ||
        original->captured != replay->captured) {
        std::cerr << "battle replay produced an unexpected result\n";
        return 1;
    }
    const auto defense = resolve({currentRule, 20, 40, 123});
    if (!defense || defense->captured || defense->attackerSurvivors != 0 || defense->defenderSurvivors != 30 ||
        resolve({0, 60, 30, 123}) || resolve({currentRule, 101, 30, 123})) {
        std::cerr << "defense or validation produced an unexpected result\n";
        return 1;
    }
    const auto firstRound = resolve({exchangeRule, 40, 40, 123});
    const auto finalRound = resolve({decisiveRule, 120, 30, 123});
    if (!firstRound || firstRound->captured || firstRound->attackerSurvivors != 30 || firstRound->defenderSurvivors != 30 ||
        !finalRound || !finalRound->captured || finalRound->attackerSurvivors != 105 ||
        resolve({decisiveRule, maximumBattleTroops + 1, 1, 123})) {
        std::cerr << "round rules produced an unexpected result\n";
        return 1;
    }
    const auto skirmish = resolve({continuousRule, 60, 30, 123});
    const auto lastStrike = resolve({continuousRule, 1, 1, 123});
    if (!skirmish || skirmish->captured || skirmish->attackerSurvivors >= 60 || skirmish->defenderSurvivors >= 30 ||
        skirmish->attackerSurvivors == 0 || skirmish->defenderSurvivors == 0 || !lastStrike ||
        lastStrike->captured || lastStrike->attackerSurvivors != 0 || lastStrike->defenderSurvivors != 1) {
        std::cerr << "continuous combat did not apply simultaneous losses\n";
        return 1;
    }
    return 0;
}
