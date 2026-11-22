#include "world.hpp"

#include <algorithm>
#include <charconv>
#include <cmath>
#include <iomanip>
#include <random>
#include <sstream>
#include <utility>

namespace slg {
namespace {

using Json = nlohmann::json;
constexpr std::uint32_t defaultMarchTroops = 40;
constexpr unsigned int reinforcementsPerSecond = 5;
constexpr std::size_t battleReportLimit = 8;

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

World::World(std::chrono::seconds seatIdleTimeout) : instance_(instanceId()), seatIdleTimeout_(seatIdleTimeout) {
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
    const std::lock_guard lock(mutex_);
    const auto now = Clock::now();
    const auto unixMs = unixMilliseconds();
    expireSeats(now);
    // 读写前先按单调时钟处理到达，玩家离线期间经过的时间也会生效。
    advance(now);
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
        const auto cursor = request.find("known_battle_id");
        const bool incremental = cursor != request.end();
        // 新客户端单独读取私有兵力。旧客户端仅在自身兵力变化时刷新完整快照。
        const auto revision = incremental ? revision_ : std::max(revision_, players_[index].reserveRevision);
        if (!sameInstance || !request.contains("known_revision") || knownRevision(request["known_revision"]) != revision) {
            reply["snapshot"] = snapshot(index, revision, now, unixMs);
            if (!incremental && !battleReports_.empty()) reply["snapshot"]["last_battle"] = battleReports_.back();
        }
        if (incremental) {
            reply["available_troops"] = players_[index].reserves;
            const auto after = sameInstance ? knownRevision(*cursor) : 0;
            for (const auto& report : battleReports_) {
                if (report["battle_id"].get<std::uint64_t>() > after) reply["battle_reports"].push_back(report);
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
    const auto count = troops == order.end() ? defaultMarchTroops : troops->get<std::uint64_t>();
    if (id.empty() || id.size() > 64 || targetId == 0 || targetId > tiles_.size() ||
        count == 0 || count > battle::maximumTroops) {
        return Result::invalid_order;
    }

    auto& player = players_[index];
    // 成功指令保留 ID，跨连接重试也只会生成一支部队。
    if (const auto prior = player.orders.find(id); prior != player.orders.end()) {
        return prior->second.target == static_cast<int>(targetId) && prior->second.troops == count
            ? Result::ok : Result::invalid_order;
    }
    if (player.marching) {
        return Result::already_marching;
    }
    const auto& target = tiles_[targetId - 1];
    if (target.owner == index + 1 || target.home != 0) {
        return Result::invalid_target;
    }
    if (count > player.reserves) {
        return Result::insufficient_troops;
    }
    // 发令时预留目标，避免同一地块发生相互覆盖的结算。
    for (const auto& other : players_) {
        if (other.marching && other.marching->target == target.id) {
            return Result::reserved;
        }
    }
    for (const auto& source : tiles_) {
        const int q = source.q - target.q, r = source.r - target.r;
        if (source.owner == index + 1 && std::max({std::abs(q), std::abs(r), std::abs(q + r)}) == 1) {
            const auto deployed = static_cast<std::uint32_t>(count);
            player.orders.emplace(id, PlacedOrder{target.id, deployed});
            const bool replenishing = player.reserves < battle::maximumTroops;
            player.reserves -= deployed;
            if (!replenishing && player.reserves < battle::maximumTroops) player.lastReinforcement = now;
            player.marching = March{source.id, target.id, deployed, now + std::chrono::seconds(3), std::nullopt};
            revision_ = nextRevision_++;
            return Result::ok;
        }
    }
    return Result::invalid_target;
}

void World::expireSeats(Clock::time_point now) {
    // 敌对行军被受理后，双方席位保留到战斗结束，避免离线使驻军提前消失。
    std::array<bool, 2> fighting{};
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
        // 回收该玩家的地块、行军预留和指令记录，其他玩家的进度继续保留。
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
        auto& active = player.marching;
        if (active && now >= active->arrives) {
            auto& target = tiles_[active->target - 1];
            if (target.owner == 0) {
                // 中立地块直接占领，已出征的士兵留下驻守。
                target.owner = index + 1;
                target.garrison = active->troops;
                active.reset();
                revision_ = nextRevision_++;
            } else if (!active->battle) {
                // 同一场战斗只生成一次种子，重试沿用到达时的快照。
                std::random_device random;
                const auto seed = (static_cast<std::uint64_t>(random()) << 32) | random();
                active->battle = BattleTask{nextBattleId_++, target.id, index + 1, target.owner,
                                             {battle::currentRule, active->troops, target.garrison, seed}};
            }
        }
    }
}

std::optional<BattleTask> World::pendingBattle() {
    const std::lock_guard lock(mutex_);
    const auto now = Clock::now();
    expireSeats(now);
    advance(now);
    std::optional<BattleTask> next;
    // 按战斗 ID 顺序提交，重试时后续战斗也不能越过战报游标。
    for (const auto& player : players_) {
        const auto& march = player.marching;
        if (march && march->battle && (!next || march->battle->id < next->id)) {
            next = march->battle;
        }
    }
    return next;
}

void World::settleBattle(const BattleTask& task, const battle::Result& result) {
    const std::lock_guard lock(mutex_);
    const auto now = Clock::now();
    // 重新核对战斗 ID 和领地归属后才提交，过期席位在提交之后统一处理。
    auto& player = players_[task.attacker - 1];
    auto& target = tiles_[task.target - 1];
    if (!player.marching || !player.marching->battle || player.marching->battle->id != task.id ||
        target.owner != task.defender || target.garrison != task.request.defenderTroops ||
        result.attackerSurvivors > task.request.attackerTroops || result.defenderSurvivors > task.request.defenderTroops ||
        (result.captured ? (result.attackerSurvivors == 0 || result.defenderSurvivors != 0) : result.defenderSurvivors == 0)) {
        return;
    }
    // 先补齐回包前到期的兵力，再应用战斗返还。
    advance(now);
    if (result.captured) {
        target.owner = task.attacker;
        target.garrison = result.attackerSurvivors;
    } else {
        target.garrison = result.defenderSurvivors;
        player.reserves += result.attackerSurvivors;
    }
    player.marching.reset();
    battleReports_.push_back({{"battle_id", task.id}, {"target_tile_id", task.target},
                              {"attacker", task.attacker}, {"defender", task.defender},
                              {"attacker_troops", task.request.attackerTroops}, {"defender_troops", task.request.defenderTroops},
                              {"attacker_survivors", result.attackerSurvivors}, {"defender_survivors", result.defenderSurvivors},
                              {"captured", result.captured}, {"rule_version", task.request.rule}, {"seed", task.request.seed}});
    if (battleReports_.size() > battleReportLimit) battleReports_.pop_front();
    revision_ = nextRevision_++;
    expireSeats(now);
}

Json World::snapshot(int index, std::uint64_t revision, Clock::time_point now, std::int64_t unixMs) const {
    Json tiles = Json::array(), marches = Json::array();
    for (const auto& tile : tiles_) {
        tiles.push_back({{"id", tile.id}, {"q", tile.q}, {"r", tile.r}, {"owner", tile.owner},
                         {"home", tile.home}, {"garrison", tile.garrison}});
    }
    for (int i = 0; i < static_cast<int>(players_.size()); ++i) {
        if (const auto& active = players_[i].marching) {
            const auto remaining = std::max<std::int64_t>(0,
                std::chrono::duration_cast<std::chrono::milliseconds>(active->arrives - now).count());
            marches.push_back({{"player", i + 1}, {"source_tile_id", active->source},
                               {"target_tile_id", active->target}, {"arrives_at_ms", unixMs + remaining},
                               {"troops", active->troops}, {"hostile", tiles_[active->target - 1].owner != 0}});
        }
    }
    return Json{{"instance", instance_}, {"revision", revision}, {"your_player", index + 1},
                {"tiles", std::move(tiles)}, {"marches", std::move(marches)},
                {"available_troops", players_[index].reserves}, {"troop_capacity", battle::maximumTroops}};
}

}  // slg 命名空间
