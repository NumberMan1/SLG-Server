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

std::uint64_t syncRevisionOffset(const std::string& instance) {
    std::uint64_t offset = 0;
    std::from_chars(instance.data(), instance.data() + 15, offset, 16);
    return offset;
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

battle::Terrain World::Tile::terrain() const {
    // 从轴坐标推导地貌，扩图追加外环时已有据点的地貌保持一致。
    if (home != 0) return battle::Terrain::plains;
    const int east = 2 * q + r;
    if (std::abs(east) <= 1) return battle::Terrain::ford;
    if (east < 0 && r < 0) return battle::Terrain::highland;
    if (east > 0 && r > 0) return battle::Terrain::woods;
    return battle::Terrain::plains;
}

World::World(std::chrono::seconds seatIdleTimeout, int radius)
    : instance_(instanceId()), syncRevisionOffset_(syncRevisionOffset(instance_)), seatIdleTimeout_(seatIdleTimeout), radius_(radius) {
    if (radius < 2 || radius > 7) throw std::invalid_argument("SLG_WORLD_RADIUS must be between 2 and 7");
    // 保留原始 19 个地块的 ID，扩图时逐环追加，已有行军、战斗和存档仍使用原 ID。
    for (int ring = 2; ring <= radius; ++ring) {
        for (int r = -ring; r <= ring; ++r) {
            for (int q = -ring; q <= ring; ++q) {
                const auto distance = std::max({std::abs(q), std::abs(r), std::abs(q + r)});
                if (distance > ring || (ring > 2 && distance < ring)) continue;
                Tile tile{static_cast<int>(tiles_.size()) + 1, q, r};
                if (r == 0 && (q == -2 || q == 2)) tile.home = q == -2 ? 1 : 2;
                tiles_.push_back(tile);
            }
        }
    }
}

World::Result World::update(const std::string& account, const Json& request) {
    const auto now = Clock::now();
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
        const auto known = request.find("known_instance");
        result = known != request.end() && known->is_string() && !known->get_ref<const std::string&>().empty()
            ? placeOrder(index, *order, now, *known == instance_) : Result::world_changed;
    }
    return result;
}

Json World::read(const std::string& account, const Json& request, std::optional<Result> result) const {
    const auto index = findPlayer(account);
    return reply(index, result.value_or(index >= 0 ? Result::ok : Result::full), request, Clock::now());
}

bool World::hasPlayer(const std::string& account) const { return findPlayer(account) >= 0; }
bool World::hasOpenSeat() const { return findPlayer("") >= 0; }

void World::touch(const std::string& account) {
    if (const auto index = findPlayer(account); index >= 0) {
        players_[index].lastSeen = Clock::now();
    }
}

Json World::reply(int index, Result result, const Json& request, Clock::time_point now) const {
    const auto unixMs = unixMilliseconds();
    Json reply{{"result", static_cast<int>(result)}, {"server_time_ms", unixMs}};
    if (index >= 0) {
        const bool sameInstance = request.contains("known_instance") && request["known_instance"] == instance_;
        const auto battleCursor = request.find("known_battle_id");
        const auto reportCursor = request.find("known_report_id");
        const bool incremental = battleCursor != request.end() || reportCursor != request.end();
        const auto firstPending = engagements_.empty() ? nextBattleId_ : engagements_.begin()->first;
        const auto reserves = reservesAt(players_[index], now);
        // 旧客户端用地图版本和补兵进度共同判断快照。超过补兵上限的变化均来自地图事件。
        // 重启会重新计算绝对到点时间，因此同步版本包含本进程标记；存档实例仍用于原指令重试。
        const auto layoutOffset = static_cast<std::uint64_t>(radius_ - 2) << 52;
        const auto mapRevision = revision_ + layoutOffset;
        const auto revision = syncRevisionOffset_ + (incremental ? mapRevision
            : mapRevision * (battle::maximumTroops + 1) + std::min(reserves, battle::maximumTroops));
        const auto tileRevision = tileRevision_ + layoutOffset;
        const auto knownTiles = request.find("known_tile_revision");
        const bool tracksTiles = knownTiles != request.end();
        const bool reuseTiles = tracksTiles && sameInstance && knownRevision(*knownTiles) == tileRevision;
        if (!sameInstance || !request.contains("known_revision") || knownRevision(request["known_revision"]) != revision ||
            (tracksTiles && !reuseTiles)) {
            reply["snapshot"] = snapshot(index, revision, reserves, now, unixMs, !reuseTiles);
            if (tracksTiles) reply["snapshot"]["tile_revision"] = tileRevision;
        }
        if (incremental) {
            reply["available_troops"] = reserves;
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
    players_[index].seat = nextRevision_;
    players_[index].orders = std::make_shared<OrderHistory>();
    players_[index].lastReinforcement = now;
    for (auto& tile : tiles_) {
        if (tile.home == index + 1) {
            setTileState(tile, index + 1, tile.garrison);
            break;
        }
    }
    revision_ = nextRevision_++;
    return index;
}

World::Result World::placeOrder(int index, const Json& order, Clock::time_point now, bool sameInstance) {
    const auto rejected = sameInstance ? Result::invalid_order : Result::world_changed;
    if (!order.is_object() || !order.contains("command_id") || !order["command_id"].is_string() ||
        !order.contains("target_tile_id") || !order["target_tile_id"].is_number_unsigned()) {
        return rejected;
    }
    const auto& id = order["command_id"].get_ref<const std::string&>();
    const auto targetId = order["target_tile_id"].get<std::uint64_t>();
    const auto troops = order.find("troops");
    if (troops != order.end() && !troops->is_number_unsigned()) {
        return rejected;
    }
    const auto rounds = order.find("round_battle");
    const auto continuous = order.find("continuous_battle");
    if ((rounds != order.end() && !rounds->is_boolean()) || (continuous != order.end() && !continuous->is_boolean()))
        return rejected;
    const bool roundBattle = rounds != order.end() && rounds->get<bool>();
    const bool continuousBattle = continuous != order.end() && continuous->get<bool>();
    const auto battleId = order.find("reinforce_battle_id");
    const auto reinforcement = battleId == order.end() ? 0 : knownRevision(*battleId);
    const auto retreatId = order.find("retreat_battle_id");
    const auto withdrawing = retreatId == order.end() ? 0 : knownRevision(*retreatId);
    const auto typeField = order.find("troop_type");
    if (typeField != order.end() && !typeField->is_number_unsigned()) return rejected;
    const auto kind = typeField == order.end() ? 0 : typeField->get<std::uint64_t>();
    const auto count = withdrawing != 0 ? 0 : troops == order.end() ? defaultMarchTroops : troops->get<std::uint64_t>();
    if (id.empty() || id.size() > 64 || targetId == 0 || targetId > tiles_.size() || count > battle::maximumTroops ||
        (roundBattle && continuousBattle) || (battleId != order.end() && reinforcement == 0) ||
        (retreatId != order.end() && withdrawing == 0) || (count == 0 && withdrawing == 0) ||
        ((reinforcement || withdrawing) && (roundBattle || continuousBattle)) ||
        (withdrawing != 0 && (reinforcement != 0 || troops != order.end())) ||
        kind > static_cast<std::uint32_t>(battle::TroopType::cavalry) || (kind != 0 && !continuousBattle)) {
        return rejected;
    }
    const auto mode = continuousBattle ? CombatMode::continuous : roundBattle ? CombatMode::rounds : CombatMode::instant;
    const PlacedOrder placed{static_cast<int>(targetId), static_cast<std::uint32_t>(count), mode, reinforcement, withdrawing,
                             static_cast<battle::TroopType>(kind)};

    auto& player = players_[index];
    // 其他席位回收会更换世界实例。原席位可以确认相同的已提交指令，旧实例不能执行新指令。
    if (const auto prior = player.orders->find(id); prior != player.orders->end()) {
        return prior->second == placed ? Result::ok : rejected;
    }
    if (!sameInstance) return Result::world_changed;
    if (withdrawing != 0) return retreat(index, id, placed, now);
    if (player.marching) {
        return Result::already_marching;
    }
    const auto& target = tiles_[targetId - 1];
    auto marchingType = placed.troopType;
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
        marchingType = attacker ? task.request.attackerType : task.request.defenderType;
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
    if (count > reservesAt(player, now)) return Result::insufficient_troops;
    for (const auto& source : tiles_) {
        const int q = source.q - target.q, r = source.r - target.r;
        if (source.owner == index + 1 && std::max({std::abs(q), std::abs(r), std::abs(q + r)}) == 1) {
            rememberOrder(player, id, placed);
            settleReserves(player, now);
            const bool replenishing = player.reserves < battle::maximumTroops;
            player.reserves -= placed.troops;
            if (!replenishing && player.reserves < battle::maximumTroops) player.lastReinforcement = now;
            revision_ = nextRevision_++;
            player.marching = March{source.id, target.id, placed.troops, now + std::chrono::seconds(3), revision_, mode,
                                    reinforcement, marchingType};
            return Result::ok;
        }
    }
    return Result::invalid_target;
}

World::Result World::retreat(int index, const std::string& commandId, const PlacedOrder& order, Clock::time_point now) {
    const auto found = engagements_.find(order.retreat);
    if (found == engagements_.end() || !found->second.continuous()) return Result::battle_closed;
    const auto& engagement = found->second;
    const auto& task = engagement.task;
    const int player = index + 1;
    if (task.target != order.target || (task.attacker != player && task.defender != player)) return Result::battle_closed;

    // 处理指令前已经结算到期交锋，撤退使用当时仍在场的全部兵力。
    const battle::Result result{task.request.attackerTroops + engagement.attackerWaiting,
                                task.request.defenderTroops + engagement.defenderWaiting, task.defender == player};
    rememberOrder(players_[index], commandId, order);
    finishBattle(engagement, result, now, player);
    engagements_.erase(found);
    return Result::ok;
}

SavedOrder World::savedOrder(std::uint64_t seat, const std::string& id, const PlacedOrder& order) {
    return {seat, id, order.target, order.troops, static_cast<int>(order.combatMode), order.reinforcement, order.retreat,
            static_cast<std::uint32_t>(order.troopType)};
}

void World::rememberOrder(Player& player, const std::string& id, const PlacedOrder& order) {
    player.orders->emplace(id, order);
    newOrder_ = savedOrder(player.seat, id, order);
}

void World::setTileState(Tile& tile, int owner, std::uint32_t garrison, battle::TroopType type) {
    if (garrison == 0) type = battle::TroopType::militia;
    if (tile.owner == owner && tile.garrison == garrison && tile.garrisonType == type) return;
    tile.owner = owner;
    tile.garrison = garrison;
    tile.garrisonType = type;
    changedTiles_.insert(tile.id);
    tileRevision_ = nextRevision_;
}

std::optional<SavedOrder> World::takeOrder() { return std::exchange(newOrder_, std::nullopt); }

std::array<std::uint64_t, 2> World::seats() const { return {players_[0].seat, players_[1].seat}; }

std::uint32_t World::reservesAt(const Player& player, Clock::time_point now) {
    if (player.reserves >= battle::maximumTroops || now <= player.lastReinforcement) return player.reserves;
    const auto elapsed = std::chrono::duration_cast<std::chrono::seconds>(now - player.lastReinforcement).count();
    const auto added = std::min<std::int64_t>(battle::maximumTroops - player.reserves, elapsed * reinforcementsPerSecond);
    return player.reserves + static_cast<std::uint32_t>(added);
}

void World::settleReserves(Player& player, Clock::time_point now) {
    if (player.reserves >= battle::maximumTroops || now <= player.lastReinforcement) return;
    player.reserves = reservesAt(player, now);
    player.lastReinforcement += std::chrono::duration_cast<std::chrono::seconds>(now - player.lastReinforcement);
}

void World::returnTroops(Player& player, std::uint32_t troops, Clock::time_point now) {
    settleReserves(player, now);
    player.reserves += troops;
}

bool World::recordReplenishmentSince(Clock::time_point saved, Clock::time_point now) {
    const auto changed = std::ranges::any_of(players_, [saved, now](const Player& player) {
        return !player.account.empty() && reservesAt(player, saved) != reservesAt(player, now);
    });
    if (changed) ++nextRevision_;
    return changed;
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
                setTileState(tile, 0, 0);
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
        if (const auto& march = players_[index].marching; march && now >= march->arrives) {
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
                returnTroops(player, active->troops, now);
            } else {
                auto& engagement = found->second;
                changedBattles_.insert(found->first);
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
            setTileState(target, index + 1, active->troops, active->troopType);
            active.reset();
            revision_ = nextRevision_++;
        } else {
            // 在到达时冻结首回合兵力，支持旧客户端保留原有的一次结算。
            std::random_device random;
            const auto seed = (static_cast<std::uint64_t>(random()) << 32) | random();
            const bool continuous = active->combatMode == CombatMode::continuous;
            const bool rounds = active->combatMode == CombatMode::rounds;
            const bool typed = active->troopType != battle::TroopType::militia || target.garrisonType != battle::TroopType::militia;
            const auto rule = continuous ? (typed ? battle::formationRule : battle::continuousRule) : rounds ? battle::exchangeRule :
                target.garrison > battle::maximumTroops ? battle::decisiveRule : battle::currentRule;
            const auto id = nextBattleId_++;
            engagements_.emplace(id, Engagement{{id, target.id, index + 1, target.owner,
                {rule, active->troops, target.garrison, seed, active->troopType, target.garrisonType, target.terrain()}, 1},
                continuous ? 0 : rounds ? battleRounds : 1,
                active->arrives + (continuous ? combatInterval : rounds ? roundDuration : std::chrono::seconds(0)),
                seed, active->troops, target.garrison});
            changedBattles_.insert(id);
            active.reset();
            revision_ = nextRevision_++;
        }
    }
}

void World::tick() { step(Clock::now()); }

void World::step(Clock::time_point now) {
    // 写入等待或调度延迟可能同时留下多个到期事件，按原截止时间补推进。
    for (;;) {
        auto next = now;
        for (const auto& player : players_) {
            if (player.marching) next = std::min(next, player.marching->arrives);
        }
        for (const auto& [_, engagement] : engagements_) next = std::min(next, engagement.roundEnds);
        expireSeats(next);
        // 同一时刻先接收行军，再结算交锋，否则晚到的援军可能参与先前已经到期的战损。
        advance(next);
        settleDueBattles(next);
        if (next == now) return;
    }
}

void World::settleDueBattles(Clock::time_point now) {
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
        throw std::logic_error("battle result does not match the world snapshot");
    }
    if (finalRound || result.attackerSurvivors == 0 || result.defenderSurvivors == 0) {
        finishBattle(engagement, result, now);
        engagements_.erase(current);
        expireSeats(now);
        return;
    }
    ++task.round;
    changedBattles_.insert(task.id);
    engagement.lastAttackerLosses = task.request.attackerTroops - result.attackerSurvivors;
    engagement.lastDefenderLosses = task.request.defenderTroops - result.defenderSurvivors;
    if (!engagement.continuous()) task.request.rule = task.round == engagement.rounds ? battle::decisiveRule : battle::exchangeRule;
    task.request.attackerTroops = result.attackerSurvivors + engagement.attackerWaiting;
    task.request.defenderTroops = result.defenderSurvivors + engagement.defenderWaiting;
    task.request.seed = engagement.seed ^ (task.round - 1);
    engagement.attackerReinforcements += std::exchange(engagement.attackerWaiting, 0);
    engagement.defenderReinforcements += std::exchange(engagement.defenderWaiting, 0);
    engagement.roundEnds = now + (engagement.continuous() ? combatInterval : roundDuration);
    revision_ = nextRevision_++;
}

void World::finishBattle(const Engagement& engagement, const battle::Result& result, Clock::time_point now, int retreatedPlayer) {
    const auto& task = engagement.task;
    auto& player = players_[task.attacker - 1];
    auto& target = tiles_[task.target - 1];
    changedBattles_.insert(task.id);
    if (result.captured) {
        setTileState(target, task.attacker, result.attackerSurvivors, task.request.attackerType);
        if (retreatedPlayer == task.defender) returnTroops(players_[task.defender - 1], result.defenderSurvivors, now);
    } else {
        setTileState(target, target.owner, result.defenderSurvivors, task.request.defenderType);
        returnTroops(player, result.attackerSurvivors, now);
    }
    for (auto& participant : players_) {
        // 结算时仍在路上的援军整队退回，不能撞进地块上的下一场战斗。
        if (participant.marching && participant.marching->reinforcement == task.id) {
            returnTroops(participant, participant.marching->troops, now);
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
    report.update({{"attacker_type", static_cast<std::uint32_t>(task.request.attackerType)},
                   {"defender_type", static_cast<std::uint32_t>(task.request.defenderType)},
                   {"terrain", static_cast<std::uint32_t>(task.request.terrain)}});
    if (engagement.continuous()) report["exchanges"] = task.round - (retreatedPlayer != 0 ? 1 : 0);
    // 结算顺序独立于开战顺序，正在交战的场次不会阻塞新战报。
    report["report_id"] = nextReportId_++;
    battleReports_.push_back(std::move(report));
    if (battleReports_.size() > battleReportLimit) battleReports_.pop_front();
    revision_ = nextRevision_++;
}

Json World::snapshot(int index, std::uint64_t revision, std::uint32_t reserves, Clock::time_point now, std::int64_t unixMs,
                     bool includeTiles) const {
    Json data{{"instance", instance_}, {"revision", revision}, {"your_player", index + 1},
              {"available_troops", reserves}, {"troop_capacity", battle::maximumTroops}};
    if (includeTiles) {
        auto& tiles = data["tiles"] = Json::array();
        for (const auto& tile : tiles_) {
            tiles.push_back({{"id", tile.id}, {"q", tile.q}, {"r", tile.r}, {"owner", tile.owner},
                             {"home", tile.home}, {"garrison", tile.garrison},
                             {"garrison_type", static_cast<std::uint32_t>(tile.garrisonType)},
                             {"terrain", static_cast<std::uint32_t>(tile.terrain())}});
        }
    }
    auto& marches = data["marches"] = Json::array();
    auto& battles = data["battles"] = Json::array();
    for (int i = 0; i < static_cast<int>(players_.size()); ++i) {
        if (const auto& active = players_[i].marching) {
            marches.push_back({{"player", i + 1}, {"source_tile_id", active->source},
                               {"target_tile_id", active->target}, {"arrives_at_ms", projectedTime(active->arrives, now, unixMs)},
                               {"troops", active->troops}, {"troop_type", static_cast<std::uint32_t>(active->troopType)},
                               {"hostile", active->reinforcement == 0 && tiles_[active->target - 1].owner != 0}});
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
                           {"continuous", engagement.continuous()}, {"rule_version", task.request.rule},
                           {"attacker_type", static_cast<std::uint32_t>(task.request.attackerType)},
                           {"defender_type", static_cast<std::uint32_t>(task.request.defenderType)},
                           {"terrain", static_cast<std::uint32_t>(task.request.terrain)},
                           {"last_attacker_losses", engagement.lastAttackerLosses},
                           {"last_defender_losses", engagement.lastDefenderLosses}});
    }
    return data;
}

}  // slg 命名空间
