#include "slg/battle_wire.hpp"

#include <iostream>

int main() {
    using namespace slg::battle;
    const Request attack{currentRule, 60, 30, 123};
    const auto original = resolve(attack);
    const auto decoded = wire::request(wire::encode(attack));
    const auto replay = decoded ? resolve(*decoded) : std::nullopt;
    if (!original || !replay || !original->captured || original->attackerSurvivors != 45 ||
        original->attackerSurvivors != replay->attackerSurvivors || original->defenderSurvivors != replay->defenderSurvivors ||
        original->captured != replay->captured) {
        std::cerr << "battle replay produced an unexpected result\n";
        return 1;
    }
    const auto defense = resolve({currentRule, 20, 40, 123});
    if (!defense || defense->captured || defense->attackerSurvivors != 0 || defense->defenderSurvivors != 30 ||
        wire::result(wire::encode(*defense))->defenderSurvivors != defense->defenderSurvivors ||
        resolve({currentRule + 1, 60, 30, 123}) || resolve({currentRule, 101, 30, 123})) {
        std::cerr << "defense or validation produced an unexpected result\n";
        return 1;
    }
    return 0;
}
