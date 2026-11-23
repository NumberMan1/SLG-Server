#include "world.hpp"

#include <algorithm>
#include <charconv>
#include <cmath>
#include <iomanip>
#include <random>
#include <sstream>
#include <stdexcept>
#include <tuple>
#include <utility>

namespace slg {
namespace {

using Json = nlohmann::json;
constexpr std::uint32_t defaultMarchTroops = 40;
constexpr unsigned int reinforcementsPerSecond = 5;
constexpr std::size_t battleReportLimit = 8;
constexpr unsigned int battleRounds = 3;
constexpr auto roundDuration = std::chrono::seconds(6);
constexpr auto combatInterval = std::chrono::seconds(1);

std::int64_t projectedTime(std::chrono::steady_clock::time_point deadline,
                           std::chrono::steady_clock::time_point now, std::int64_t unixMs) {
    return unixMs + std::max<std::int64_t>(0, std::chrono::duration_cast<std::chrono::milliseconds>(deadline - now).count());
}

// 实例变化会让客户端重新请求完整快照，并拒绝旧世界的行军重试。
std::string instanceId() {
    std::random_device random;
    std::ostringstream output;
    output << std::hex << std::setfill('0');
    for (int i = 0; i < 4; ++i) {
        output << std::setw(8) << static_cast<std::uint32_t>(random());
    }
    return output.str();
}

std::int64_t unixMilliseconds() {
    return std::chrono::duration_cast<std::chrono::milliseconds>(
        std::chrono::system_clock::now().time_since_epoch()).count();
}

std::uint64_t knownRevision(const Json& value) {
    // Protobuf JSON 以十进制字符串传输 uint64。
    if (const auto text = value.get_ptr<const std::string*>()) {
        std::uint64_t revision = 0;
        const char* end = text->data() + text->size();
        const auto [next, error] = std::from_chars(text->data(), end, revision);
        return error == std::errc{} && next == end ? revision : 0;
    }
    return value.is_number_unsigned() ? value.get<std::uint64_t>() : 0;
}

}  // 匿名命名空间

World::World(std::chrono::seconds seatIdleTimeout)
    : instance_(instanceId()), seatIdleTimeout_(seatIdleTimeout) {
    for (int r = -2; r <= 2; ++r) {
        for (int q = -2; q <= 2; ++q) {
            if (std::abs(q + r) > 2) {
                continue;
            }
            Tile tile{static_cast<int>(tiles_.size()) + 1, q, r};
            if (r == 0 && (q == -2 || q == 2)) {
                tile.home = q == -2 ? 1 : 2;
            }
            tiles_.push_back(tile);
        }
    }
}

Json World::update(const std::string& account, const Json& request) {
    const auto now = Clock::now();
    const auto unixMs = unixMilliseconds();
    step(now);
    const auto order = request.find("order");
    auto index = findPlayer(account);
    if (index < 0 && order == request.end()) {
        index = join(account, now);
    }
    if (index >= 0) {
        players_[index].lastSeen = now;
    }

    Result result = Result::ok;
    if (index < 0) {
        if (order == request.end()) {
            result = Result::full;
        } else {
            result = request.contains("known_instance") && request["known_instance"] == instance_
                ? Result::invalid_order : Result::world_changed;
        }
    } else if (order != request.end()) {
        result = request.contains("known_instance") && request["known_instance"] == instance_
            ? placeOrder(index, *order, now) : Result::world_changed;
    }
    Json reply{{"result", static_cast<int>(result)}, {"server_time_ms", unixMs}};
    if (index >= 0) {
        const bool sameInstance = request.contains("known_instance") && request["known_instance"] == instance_;
        const auto battleCursor = request.find("known_battle_id");
        const auto reportCursor = request.find("known_report_id");
        const bool incremental = battleCursor != request.end() || reportCursor != request.end();
        const auto firstPending = engagements_.empty() ? nextBattleId_ : engagements_.begin()->first;
        // 新客户端单独读取私有兵力。旧客户端仅在自身兵力变化时刷新完整快照。
        const auto revision = incremental ? revision_ : std::max(revision_, players_[index].reserveRevision);
        if (!sameInstance || !request.contains("known_revision") || knownRevision(request["known_revision"]) != revision) {
            reply["snapshot"] = snapshot(index, revision, now, unixMs);
        }
        if (incremental) {
            reply["available_troops"] = players_[index].reserves;
        }
        if (reportCursor != request.end()) {
            const auto after = sameInstance ? knownRevision(*reportCursor) : 0;
            for (const auto& report : battleReports_) {
                if (report["report_id"].get<std::uint64_t>() > after) reply["battle_reports"].push_back(report);
            }
        } else if (incremental || reply.contains("snapshot")) {
            // 旧游标使用开战编号，只返回不会跳过尚未结束战斗的有序结果。
            const auto after = incremental && sameInstance ? knownRevision(*battleCursor) : 0;
            std::map<std::uint64_t, const Json*> legacy;
            for (const auto& report : battleReports_) {
                const auto id = report["battle_id"].get<std::uint64_t>();
                if (after < id && id < firstPending) legacy.emplace(id, &report);
            }
            if (incremental) {
                for (const auto& [_, report] : legacy) reply["battle_reports"].push_back(*report);
            } else if (!legacy.empty()) {
                reply["snapshot"]["last_battle"] = *legacy.rbegin()->second;
            }
        }
    }
    return reply;
}

int World::findPlayer(const std::string& account) const {
    for (int index = 0; index < static_cast<int>(players_.size()); ++index) {
        if (players_[index].account == account) {
            return index;
        }
    }
    return -1;
}

int World::join(const std::string& account, Clock::time_point now) {
    const int index = findPlayer("");
    if (index < 0) {
        return -1;
    }
    players_[index].account = account;
    players_[index].lastReinforcement = now;
    for (auto& tile : tiles_) {
        if (tile.home == index + 1) {
            tile.owner = index + 1;
            break;
        }
    }
    revision_ = nextRevision_++;
    return index;
}

World::Result World::placeOrder(int index, const Json& order, Clock::time_point now) {
    if (!order.is_object() || !order.contains("command_id") || !order["command_id"].is_string() ||
        !order.contains("target_tile_id") || !order["target_tile_id"].is_number_unsigned()) {
        return Result::invalid_order;
    }
    const auto& id = order["command_id"].get_ref<const std::string&>();
    const auto targetId = order["target_tile_id"].get<std::uint64_t>();
    const auto troops = order.find("troops");
    if (troops != order.end() && !troops->is_number_unsigned()) {
        return Result::invalid_order;
    }
    const auto rounds = order.find("round_battle");
    const auto continuous = order.find("continuous_battle");
    if ((rounds != order.end() && !rounds->is_boolean()) || (continuous != order.end() && !continuous->is_boolean()))
        return Result::invalid_order;
    const bool roundBattle = rounds != order.end() && rounds->get<bool>();
    const bool continuousBattle = continuous != order.end() && continuous->get<bool>();
    const auto battleId = order.find("reinforce_battle_id");
    const auto reinforcement = battleId == order.end() ? 0 : knownRevision(*battleId);
    const auto retreatId = order.find("retreat_battle_id");
    const auto withdrawing = retreatId == order.end() ? 0 : knownRevision(*retreatId);
    const auto count = withdrawing != 0 ? 0 : troops == order.end() ? defaultMarchTroops : troops->get<std::uint64_t>();
    if (id.empty() || id.size() > 64 || targetId == 0 || targetId > tiles_.size() || count > battle::maximumTroops ||
        (roundBattle && continuousBattle) || (battleId != order.end() && reinforcement == 0) ||
        (retreatId != order.end() && withdrawing == 0) || (count == 0 && withdrawing == 0) ||
        ((reinforcement || withdrawing) && (roundBattle || continuousBattle)) ||
        (withdrawing != 0 && (reinforcement != 0 || troops != order.end()))) {
        return Result::invalid_order;
    }
    const auto mode = continuousBattle ? CombatMode::continuous : roundBattle ? CombatMode::rounds : CombatMode::instant;
    const PlacedOrder placed{static_cast<int>(targetId), static_cast<std::uint32_t>(count), mode, reinforcement, withdrawing};

    auto& player = players_[index];
    // 成功指令保留 ID，跨连接重试也只会生成一支部队。
    if (const auto prior = player.orders.find(id); prior != player.orders.end()) {
        return prior->second == placed ? Result::ok : Result::invalid_order;
    }
    if (withdrawing != 0) return retreat(index, id, placed);
    if (player.marching) {
        return Result::already_marching;
    }
    const auto& target = tiles_[targetId - 1];
    if (reinforcement != 0) {
        const auto found = engagements_.find(reinforcement);
        if (found == engagements_.end()) return Result::reinforcement_closed;
        const auto& engagement = found->second;
        const auto& task = engagement.task;
        const bool attacker = task.attacker == index + 1;
        if (task.target != target.id || (!attacker && task.defender != index + 1) || !engagement.acceptsReinforcement())
            return Result::reinforcement_closed;
        const auto occupied = attacker ? task.request.attackerTroops + engagement.attackerWaiting :
                                         task.request.defenderTroops + engagement.defenderWaiting;
        if (occupied + count > battle::maximumBattleTroops) return Result::battle_capacity;
    } else {
        if (target.owner == index + 1 || target.home != 0) return Result::invalid_target;
        for (const auto& [_, engagement] : engagements_) {
            if (engagement.task.attacker == index + 1) return Result::already_marching;
            if (engagement.task.target == target.id) return Result::reserved;
        }
        // 中立地允许争抢，已有归属的进攻目标则保留给先受理的进攻。
        if (target.owner != 0) {
            for (const auto& other : players_) {
                if (other.marching && other.marching->target == target.id) return Result::reserved;
            }
        }
    }
    if (count > player.reserves) return Result::insufficient_troops;
    for (const auto& source : tiles_) {
        const int q = source.q - target.q, r = source.r - target.r;
        if (source.owner == index + 1 && std::max({std::abs(q), std::abs(r), std::abs(q + r)}) == 1) {
            player.orders.emplace(id, placed);
            const bool replenishing = player.reserves < battle::maximumTroops;
            player.reserves -= placed.troops;
            if (!replenishing && player.reserves < battle::maximumTroops) player.lastReinforcement = now;
            revision_ = nextRevision_++;
            player.marching = March{source.id, target.id, placed.troops, now + std::chrono::seconds(3), revision_, mode, reinforcement};
            return Result::ok;
        }
    }
    return Result::invalid_target;
}

World::Result World::retreat(int index, const std::string& commandId, const PlacedOrder& order) {
    const auto found = engagements_.find(order.retreat);
    if (found == engagements_.end() || !found->second.continuous()) return Result::battle_closed;
    const auto& engagement = found->second;
    const auto& task = engagement.task;
    const int player = index + 1;
    if (task.target != order.target || (task.attacker != player && task.defender != player)) return Result::battle_closed;

    // 处理指令前已经结算到期交锋，撤退使用当时仍在场的全部兵力。
    const battle::Result result{task.request.attackerTroops + engagement.attackerWaiting,
                                task.request.defenderTroops + engagement.defenderWaiting, task.defender == player};
    players_[index].orders.emplace(commandId, order);
    finishBattle(engagement, result, player);
    engagements_.erase(found);
    return Result::ok;
}

void World::expireSeats(Clock::time_point now) {
    // 敌对行军被受理后，双方席位保留到战斗结束，避免离线使驻军提前消失。
    std::array<bool, 2> fighting{};
    for (const auto& [_, engagement] : engagements_) {
        fighting[engagement.task.attacker - 1] = true;
        fighting[engagement.task.defender - 1] = true;
    }
    for (int index = 0; index < static_cast<int>(players_.size()); ++index) {
        if (const auto& march = players_[index].marching) {
            const int defender = tiles_[march->target - 1].owner;
            if (defender != 0) {
                fighting[index] = true;
                fighting[defender - 1] = true;
            }
        }
    }
    bool recycled = false;
    for (int index = 0; index < static_cast<int>(players_.size()); ++index) {
        auto& player = players_[index];
        if (player.account.empty() || fighting[index] || now - player.lastSeen < seatIdleTimeout_) {
            continue;
        }
        // 回收该玩家的地块、行军和指令记录，其他玩家的进度继续保留。
        player = Player{};
        for (auto& tile : tiles_) {
            if (tile.owner == index + 1) {
                tile.owner = 0;
                tile.garrison = 0;
            }
        }
        recycled = true;
    }
    if (recycled) {
        // 名额重新分配后更换世界实例，旧实例的指令无法在新归属下重新提交。
        instance_ = instanceId();
        revision_ = nextRevision_++;
    }
}

void World::advance(Clock::time_point now) {
    std::array<int, 2> arrivals{};
    std::size_t arrivalCount = 0;
    for (int index = 0; index < static_cast<int>(players_.size()); ++index) {
        auto& player = players_[index];
        if (player.account.empty()) continue;
        const auto elapsed = std::chrono::duration_cast<std::chrono::seconds>(now - player.lastReinforcement);
        if (elapsed.count() > 0) {
            // 即使储备已满也消耗经过的补兵周期，避免出征后补发超额期间的兵力。
            player.lastReinforcement += elapsed;
            if (player.reserves < battle::maximumTroops) {
                const auto added = std::min<std::int64_t>(battle::maximumTroops - player.reserves,
                                                          elapsed.count() * reinforcementsPerSecond);
                player.reserves += static_cast<std::uint32_t>(added);
                player.reserveRevision = nextRevision_++;
            }
        }
        if (const auto& march = player.marching; march && now >= march->arrives) {
            arrivals[arrivalCount++] = index;
        }
    }
    // 同一轮发现两支部队到期时，也按到达时间推进。时间相同则按受理顺序。
    if (arrivalCount == 2) {
        const auto& first = *players_[arrivals[0]].marching;
        const auto& second = *players_[arrivals[1]].marching;
        if (std::tie(second.arrives, second.dispatchRevision) < std::tie(first.arrives, first.dispatchRevision)) {
            std::swap(arrivals[0], arrivals[1]);
        }
    }
    for (std::size_t position = 0; position < arrivalCount; ++position) {
        const int index = arrivals[position];
        auto& player = players_[index];
        auto& active = player.marching;
        auto& target = tiles_[active->target - 1];
        if (active->reinforcement != 0) {
            // 持续交战中的援军到达即计入战场，普通回合制则在下一回合并入。
            const auto found = engagements_.find(active->reinforcement);
            if (found == engagements_.end() || !found->second.acceptsReinforcement()) {
                player.reserves += active->troops;
            } else {
                auto& engagement = found->second;
                const bool attacking = engagement.task.attacker == index + 1;
                auto& waiting = attacking ? engagement.attackerWaiting : engagement.defenderWaiting;
                waiting += active->troops;
                if (engagement.continuous()) {
                    auto& joined = attacking ? engagement.attackerReinforcements : engagement.defenderReinforcements;
                    joined += active->troops;
                }
            }
            active.reset();
            revision_ = nextRevision_++;
        } else if (target.owner == 0) {
            // 先抵达的部队占领中立地，后到的部队读取更新后的归属和驻军。
            target.owner = index + 1;
            target.garrison = active->troops;
            active.reset();
            revision_ = nextRevision_++;
        } else {
            // 在到达时冻结首回合兵力，支持旧客户端保留原有的一次结算。
            std::random_device random;
            const auto seed = (static_cast<std::uint64_t>(random()) << 32) | random();
            const bool continuous = active->combatMode == CombatMode::continuous;
            const bool rounds = active->combatMode == CombatMode::rounds;
            const auto rule = continuous ? battle::continuousRule : rounds ? battle::exchangeRule :
                target.garrison > battle::maximumTroops ? battle::decisiveRule : battle::currentRule;
            const auto id = nextBattleId_++;
            engagements_.emplace(id, Engagement{{id, target.id, index + 1, target.owner,
                {rule, active->troops, target.garrison, seed}, 1}, continuous ? 0 : rounds ? battleRounds : 1,
                active->arrives + (continuous ? combatInterval : rounds ? roundDuration : std::chrono::seconds(0)),
                seed, active->troops, target.garrison});
            active.reset();
            revision_ = nextRevision_++;
        }
    }
}

void World::tick() { step(Clock::now()); }

void World::step(Clock::time_point now) {
    expireSeats(now);
    // 同一次推进先按到达顺序接收行军，再结算到期的本地交锋。
    advance(now);
    for (auto cursor = engagements_.begin(); cursor != engagements_.end();) {
        const auto current = cursor++;
        auto& engagement = current->second;
        if (engagement.roundEnds > now) continue;
        if (engagement.continuous()) {
            engagement.task.request.attackerTroops += std::exchange(engagement.attackerWaiting, 0);
            engagement.task.request.defenderTroops += std::exchange(engagement.defenderWaiting, 0);
        }
        const auto result = battle::resolve(engagement.task.request);
        if (!result) throw std::logic_error("invalid local battle snapshot");
        applyBattleResult(current, *result, now);
    }
}

void World::applyBattleResult(Engagements::iterator current, const battle::Result& result, Clock::time_point now) {
    auto& engagement = current->second;
    auto& task = engagement.task;
    const auto& target = tiles_[task.target - 1];
    const bool finalRound = task.round == engagement.rounds;
    if (target.owner != task.defender || target.garrison != engagement.initialDefender ||
        result.attackerSurvivors > task.request.attackerTroops || result.defenderSurvivors > task.request.defenderTroops ||
        (!engagement.continuous() && !finalRound && (result.captured || result.attackerSurvivors == 0)) ||
        (result.captured ? (result.attackerSurvivors == 0 || result.defenderSurvivors != 0) : result.defenderSurvivors == 0)) {
        return;
    }
    if (finalRound || result.attackerSurvivors == 0 || result.defenderSurvivors == 0) {
        finishBattle(engagement, result);
        engagements_.erase(current);
        expireSeats(now);
        return;
    }
    ++task.round;
    const auto rule = engagement.continuous() ? battle::continuousRule :
        task.round == engagement.rounds ? battle::decisiveRule : battle::exchangeRule;
    task.request = {rule, result.attackerSurvivors + engagement.attackerWaiting,
                    result.defenderSurvivors + engagement.defenderWaiting, engagement.seed ^ (task.round - 1)};
    engagement.attackerReinforcements += std::exchange(engagement.attackerWaiting, 0);
    engagement.defenderReinforcements += std::exchange(engagement.defenderWaiting, 0);
    engagement.roundEnds = now + (engagement.continuous() ? combatInterval : roundDuration);
    revision_ = nextRevision_++;
}

void World::finishBattle(const Engagement& engagement, const battle::Result& result, int retreatedPlayer) {
    const auto& task = engagement.task;
    auto& player = players_[task.attacker - 1];
    auto& target = tiles_[task.target - 1];
    if (result.captured) {
        target.owner = task.attacker;
        target.garrison = result.attackerSurvivors;
        if (retreatedPlayer == task.defender) players_[task.defender - 1].reserves += result.defenderSurvivors;
    } else {
        target.garrison = result.defenderSurvivors;
        player.reserves += result.attackerSurvivors;
    }
    for (auto& participant : players_) {
        // 结算时仍在路上的援军整队退回，不能撞进地块上的下一场战斗。
        if (participant.marching && participant.marching->reinforcement == task.id) {
            participant.reserves += participant.marching->troops;
            participant.marching.reset();
        }
    }
    Json report{{"battle_id", task.id}, {"target_tile_id", task.target},
                              {"attacker", task.attacker}, {"defender", task.defender},
                              {"attacker_troops", engagement.initialAttacker + engagement.attackerReinforcements},
                              {"defender_troops", engagement.initialDefender + engagement.defenderReinforcements},
                              {"attacker_survivors", result.attackerSurvivors}, {"defender_survivors", result.defenderSurvivors},
                              {"captured", result.captured}, {"rule_version", task.request.rule}, {"seed", engagement.seed}};
    if (engagement.rounds != 1) {
        report.update({{"rounds", engagement.rounds}, {"attacker_reinforcements", engagement.attackerReinforcements},
                       {"defender_reinforcements", engagement.defenderReinforcements}});
    }
    if (retreatedPlayer != 0) report["retreated_player"] = retreatedPlayer;
    // 结算顺序独立于开战顺序，正在交战的场次不会阻塞新战报。
    report["report_id"] = nextReportId_++;
    battleReports_.push_back(std::move(report));
    if (battleReports_.size() > battleReportLimit) battleReports_.pop_front();
    revision_ = nextRevision_++;
}

Json World::snapshot(int index, std::uint64_t revision, Clock::time_point now, std::int64_t unixMs) const {
    Json tiles = Json::array(), marches = Json::array(), battles = Json::array();
    for (const auto& tile : tiles_) {
        tiles.push_back({{"id", tile.id}, {"q", tile.q}, {"r", tile.r}, {"owner", tile.owner},
                         {"home", tile.home}, {"garrison", tile.garrison}});
    }
    for (int i = 0; i < static_cast<int>(players_.size()); ++i) {
        if (const auto& active = players_[i].marching) {
            marches.push_back({{"player", i + 1}, {"source_tile_id", active->source},
                               {"target_tile_id", active->target}, {"arrives_at_ms", projectedTime(active->arrives, now, unixMs)},
                               {"troops", active->troops}, {"hostile", active->reinforcement == 0 && tiles_[active->target - 1].owner != 0}});
            if (active->reinforcement != 0) marches.back()["reinforce_battle_id"] = active->reinforcement;
        }
    }
    for (const auto& [id, engagement] : engagements_) {
        if (engagement.rounds == 1) continue;
        const auto& task = engagement.task;
        battles.push_back({{"battle_id", id}, {"target_tile_id", task.target}, {"attacker", task.attacker},
                           {"defender", task.defender}, {"round", task.round}, {"rounds", engagement.rounds},
                           {"round_ends_at_ms", projectedTime(engagement.roundEnds, now, unixMs)},
                           {"attacker_troops", task.request.attackerTroops + (engagement.continuous() ? engagement.attackerWaiting : 0)},
                           {"defender_troops", task.request.defenderTroops + (engagement.continuous() ? engagement.defenderWaiting : 0)},
                           {"attacker_waiting", engagement.continuous() ? 0 : engagement.attackerWaiting},
                           {"defender_waiting", engagement.continuous() ? 0 : engagement.defenderWaiting},
                           {"continuous", engagement.continuous()}});
    }
    return Json{{"instance", instance_}, {"revision", revision}, {"your_player", index + 1},
                {"tiles", std::move(tiles)}, {"marches", std::move(marches)}, {"battles", std::move(battles)},
                {"available_troops", players_[index].reserves}, {"troop_capacity", battle::maximumTroops}};
}

}  // slg 命名空间
