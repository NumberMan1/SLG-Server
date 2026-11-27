#include "world.hpp"

#include <algorithm>
#include <limits>
#include <stdexcept>
#include <utility>

namespace slg {
namespace {

using Json = nlohmann::json;
constexpr unsigned snapshotFormat = 3;
constexpr unsigned eventFormat = 1;
constexpr std::int64_t timerRange = 86'400'000'000'000LL;

battle::TroopType savedTroopType(const Json& entry, const char* field) {
    const auto value = entry.value(field, 0u);
    if (value > static_cast<unsigned>(battle::TroopType::cavalry)) throw std::runtime_error("invalid saved troop type");
    return static_cast<battle::TroopType>(value);
}

}  // 匿名命名空间

std::int64_t World::logicalTime(Clock::time_point time) const {
    return std::chrono::duration_cast<std::chrono::nanoseconds>(time - logicalEpoch_).count();
}

Json World::persistedHeader(Clock::time_point at) const {
    return {{"clock_ns", logicalTime(at)}, {"instance", instance_}, {"revision", revision_}, {"map_tiles", tiles_.size()},
            {"tile_revision", tileRevision_},
            {"next_revision", nextRevision_}, {"next_battle", nextBattleId_}, {"next_report", nextReportId_}};
}

Json World::persistedPlayer(Player player, Clock::time_point at) const {
    if (player.account.empty()) return nullptr;
    settleReserves(player, at);
    // 保留已有格式字段，旧存档中的兵力和补兵基准可直接继续使用。
    Json entry{{"account", player.account}, {"seat", player.seat}, {"reserves", player.reserves},
               {"reserve_revision", 0}};
    if (player.reserves < battle::maximumTroops) entry["last_reinforcement_ns"] = logicalTime(player.lastReinforcement);
    if (const auto& march = player.marching) {
        entry["march"] = {{"source", march->source}, {"target", march->target}, {"troops", march->troops},
                          {"arrives_ns", logicalTime(march->arrives)}, {"sequence", march->dispatchRevision},
                          {"mode", static_cast<int>(march->combatMode)}, {"reinforcement", march->reinforcement},
                          {"troop_type", static_cast<unsigned>(march->troopType)}};
    }
    return entry;
}

Json World::persistedBattle(const Engagement& battle) const {
    const auto& task = battle.task;
    return {{"id", task.id}, {"target", task.target}, {"attacker", task.attacker}, {"defender", task.defender},
            {"rule", task.request.rule}, {"attacker_troops", task.request.attackerTroops},
            {"defender_troops", task.request.defenderTroops}, {"round_seed", task.request.seed},
            {"round", task.round}, {"rounds", battle.rounds}, {"round_ends_ns", logicalTime(battle.roundEnds)},
            {"seed", battle.seed}, {"initial_attacker", battle.initialAttacker},
            {"initial_defender", battle.initialDefender}, {"attacker_waiting", battle.attackerWaiting},
            {"defender_waiting", battle.defenderWaiting}, {"attacker_reinforcements", battle.attackerReinforcements},
            {"defender_reinforcements", battle.defenderReinforcements},
            {"attacker_type", static_cast<unsigned>(task.request.attackerType)},
            {"defender_type", static_cast<unsigned>(task.request.defenderType)},
            {"terrain", static_cast<unsigned>(task.request.terrain)},
            {"last_attacker_losses", battle.lastAttackerLosses}, {"last_defender_losses", battle.lastDefenderLosses}};
}

std::string World::save(Clock::time_point at) const {
    auto data = persistedHeader(at);
    data.update({{"format", snapshotFormat}, {"tiles", Json::array()}, {"players", Json::array()},
                 {"battles", Json::array()}, {"reports", battleReports_}});
    for (const auto& tile : tiles_) {
        data["tiles"].push_back({{"id", tile.id}, {"q", tile.q}, {"r", tile.r}, {"owner", tile.owner}, {"garrison", tile.garrison},
                                {"garrison_type", static_cast<unsigned>(tile.garrisonType)}});
    }
    for (const auto& player : players_) data["players"].push_back(persistedPlayer(player, at));
    for (const auto& [_, battle] : engagements_) data["battles"].push_back(persistedBattle(battle));
    return data.dump();
}

std::string World::event(const World& previous, Clock::time_point at, const std::string& writer) const {
    auto data = persistedHeader(at);
    data.update({{"event_format", eventFormat}, {"writer", writer}});
    for (const auto id : changedTiles_) {
        const auto index = id - 1;
        const auto& tile = tiles_[index];
        data["tiles"].push_back({index, {{"owner", tile.owner}, {"garrison", tile.garrison},
                                        {"garrison_type", static_cast<unsigned>(tile.garrisonType)}}});
    }
    for (std::size_t index = 0; index < players_.size(); ++index) {
        const auto& player = players_[index];
        const auto& old = previous.players_[index];
        if (player.seat != old.seat || player.reserves != old.reserves ||
            (player.reserves < battle::maximumTroops && player.lastReinforcement != old.lastReinforcement) ||
            player.marching != old.marching) {
            data["players"].push_back({index, persistedPlayer(player, at)});
        }
    }
    for (const auto id : changedBattles_) {
        const auto battle = engagements_.find(id);
        data["battles"].push_back({id, battle == engagements_.end() ? Json(nullptr) : persistedBattle(battle->second)});
    }
    for (const auto& report : battleReports_) {
        if (report.at("report_id").get<std::uint64_t>() >= previous.nextReportId_) data["reports"].push_back(report);
    }
    return data.dump();
}

void World::publish(World& previous) {
    // 已提交读视图只复制这次推进涉及的地块与交战，以及固定大小的玩家状态和计数器。
    for (const auto id : changedTiles_) previous.tiles_[id - 1] = tiles_[id - 1];
    for (const auto id : changedBattles_) {
        const auto battle = engagements_.find(id);
        if (battle == engagements_.end()) previous.engagements_.erase(id);
        else previous.engagements_.insert_or_assign(id, battle->second);
    }
    previous.players_ = players_;
    previous.instance_ = instance_;
    previous.revision_ = revision_;
    previous.tileRevision_ = tileRevision_;
    previous.nextRevision_ = nextRevision_;
    previous.nextBattleId_ = nextBattleId_;
    if (previous.nextReportId_ != nextReportId_) {
        previous.battleReports_ = battleReports_;
        previous.nextReportId_ = nextReportId_;
    }
    clearChanges();
}

void World::clearChanges() {
    changedTiles_.clear();
    changedBattles_.clear();
}

World::RestoreState World::restore(std::string_view payload, const std::function<void(const ApplyEvent&)>& readEvents, bool journaled) {
    auto data = Json::parse(payload);
    const auto format = data.at("format").get<unsigned>();
    const bool legacy = format == 1;
    if ((!legacy && format != 2 && format != snapshotFormat) || (journaled && format != snapshotFormat)) {
        throw std::runtime_error("unsupported world snapshot format");
    }
    // 旧档尚未区分地块版本，取当前版本作为基准，再沿旧事件中的地块变化推进。
    if (!data.contains("tile_revision")) data["tile_revision"] = data.at("revision");
    auto& savedTiles = data.at("tiles");
    if (!savedTiles.is_array() || savedTiles.size() < 19) throw std::runtime_error("invalid world tiles in snapshot");
    if (savedTiles.size() > tiles_.size()) throw WorldLayoutError("world snapshot does not fit the configured map");
    for (std::size_t index = 0; index < savedTiles.size(); ++index) {
        const auto& saved = savedTiles[index];
        const auto& tile = tiles_[index];
        if (saved.contains("id") && (saved.at("id") != tile.id || saved.at("q") != tile.q || saved.at("r") != tile.r)) {
            throw std::runtime_error("world snapshot tile layout changed");
        }
    }
    // 旧快照可能比事件流中的地图小。先补齐扩展地块，再回放对新区域的事件。
    while (savedTiles.size() < tiles_.size()) savedTiles.push_back({{"owner", 0}, {"garrison", 0}});
    // 回放已经提交的对象变更，避免重启时用当前规则或随机数重新计算历史战果。
    if (readEvents) {
        std::map<std::uint64_t, Json> battles;
        for (auto& battle : data.at("battles")) battles.emplace(battle.at("id").get<std::uint64_t>(), std::move(battle));
        readEvents([&](std::string_view body) {
            const auto change = Json::parse(body);
            if (change.value("map_tiles", tiles_.size()) > tiles_.size()) {
                throw WorldLayoutError("world event requires a larger map");
            }
            if (change.at("event_format") != eventFormat || change.at("clock_ns") < data.at("clock_ns") ||
                change.at("next_revision") <= data.at("next_revision")) {
                throw std::runtime_error("invalid world event order");
            }
            for (const auto* field : {"clock_ns", "instance", "revision", "next_revision", "next_battle", "next_report"}) {
                data[field] = change.at(field);
            }
            if (change.contains("tile_revision")) data["tile_revision"] = change.at("tile_revision");
            else if (change.contains("tiles")) data["tile_revision"] = change.at("revision");
            for (const auto* field : {"tiles", "players"}) {
                if (const auto records = change.find(field); records != change.end()) {
                    for (const auto& record : *records) data.at(field).at(record.at(0).get<std::size_t>()) = record.at(1);
                }
            }
            if (const auto records = change.find("battles"); records != change.end()) {
                for (const auto& record : *records) {
                    const auto id = record.at(0).get<std::uint64_t>();
                    if (record.at(1).is_null()) battles.erase(id);
                    else battles[id] = record.at(1);
                }
            }
            if (const auto records = change.find("reports"); records != change.end()) {
                auto& reports = data.at("reports");
                for (const auto& report : *records) {
                    reports.push_back(report);
                    if (reports.size() > battleReportLimit) reports.erase(reports.begin());
                }
            }
        });
        auto& list = data.at("battles");
        list = Json::array();
        for (auto& [_, battle] : battles) list.push_back(std::move(battle));
    }
    const auto& tiles = data.at("tiles");
    const auto& players = data.at("players");
    const auto& battles = data.at("battles");
    if (!players.is_array() || players.size() != players_.size() || !battles.is_array()) {
        throw std::runtime_error("unsupported world snapshot");
    }
    // 停机期间暂停世界。从最后一个持久事件的世界时间重建单调时钟的基点。
    const auto now = Clock::now();
    const auto clock = format == snapshotFormat ? data.at("clock_ns").get<std::int64_t>() : 0;
    if (clock < 0 || clock > std::numeric_limits<std::int64_t>::max() - timerRange) throw std::runtime_error("invalid saved world clock");
    logicalEpoch_ = now - std::chrono::duration_cast<Clock::duration>(std::chrono::nanoseconds(clock));
    const auto timeAt = [now, legacy, clock](const Json& entry, const std::string& field) {
        const auto offset = entry.at(field + (legacy ? "_ms" : "_ns")).get<std::int64_t>();
        const auto limit = legacy ? timerRange / 1'000'000 : timerRange;
        if (offset < clock - limit || offset > clock + limit) throw std::runtime_error("invalid world timer");
        const auto elapsed = std::chrono::nanoseconds(legacy ? offset * 1'000'000 : offset - clock);
        return now + std::chrono::duration_cast<Clock::duration>(elapsed);
    };
    const auto combatMode = [](const Json& data) {
        const auto mode = data.at("mode").get<int>();
        if (mode < static_cast<int>(CombatMode::instant) || mode > static_cast<int>(CombatMode::continuous)) {
            throw std::runtime_error("invalid combat mode in world snapshot");
        }
        return static_cast<CombatMode>(mode);
    };
    const auto tileID = [this](const Json& value) {
        const auto id = value.get<int>();
        if (id < 1 || id > static_cast<int>(tiles_.size())) throw std::runtime_error("invalid saved tile ID");
        return id;
    };

    instance_ = data.at("instance").get<std::string>();
    revision_ = data.at("revision").get<std::uint64_t>();
    tileRevision_ = data.at("tile_revision").get<std::uint64_t>();
    nextRevision_ = data.at("next_revision").get<std::uint64_t>();
    nextBattleId_ = data.at("next_battle").get<std::uint64_t>();
    nextReportId_ = data.at("next_report").get<std::uint64_t>();
    if (instance_.empty() || tileRevision_ == 0 || tileRevision_ > revision_ ||
        nextRevision_ <= revision_ || nextBattleId_ == 0 || nextReportId_ == 0) {
        throw std::runtime_error("invalid world snapshot revision");
    }
    for (std::size_t index = 0; index < tiles_.size(); ++index) {
        auto& tile = tiles_[index];
        tile.owner = tiles[index].at("owner").get<int>();
        tile.garrison = tiles[index].at("garrison").get<std::uint32_t>();
        tile.garrisonType = savedTroopType(tiles[index], "garrison_type");
        if (tile.owner < 0 || tile.owner > static_cast<int>(players_.size())) throw std::runtime_error("invalid saved owner");
    }
    for (std::size_t index = 0; index < players_.size(); ++index) {
        auto& player = players_[index];
        player = Player{};
        const auto& entry = players[index];
        if (entry.is_null()) continue;
        player.account = entry.at("account").get<std::string>();
        // 旧版没有席位版本。当前席位序号小于后续的新版本，可用于迁移。
        player.seat = legacy ? index + 1 : entry.at("seat").get<std::uint64_t>();
        if (player.seat == 0 || player.seat >= nextRevision_ || (index > 0 && player.seat == players_[0].seat)) {
            throw std::runtime_error("invalid saved seat revision");
        }
        player.orders = std::make_shared<OrderHistory>();
        // 恢复后从重新开放连接起计算离线宽限期，读请求的活跃时间无需持续写库。
        player.lastSeen = now;
        player.reserves = entry.at("reserves").get<std::uint32_t>();
        // 储备达到上限时没有待恢复的补兵时钟，下次扣到上限以下会重新计时。
        if (format == snapshotFormat && player.reserves < battle::maximumTroops &&
            entry.at("last_reinforcement_ns").get<std::int64_t>() < clock - timerRange) {
            // 长时间只回放其他对象的事件时，旧补兵基准可能已足够补满。
            player.reserves = battle::maximumTroops;
        }
        player.lastReinforcement = format != snapshotFormat || player.reserves < battle::maximumTroops
            ? timeAt(entry, "last_reinforcement") : now;
        if (legacy) {
            for (const auto& [id, order] : entry.at("orders").items()) {
                player.orders->emplace(id, PlacedOrder{tileID(order.at("target")), order.at("troops").get<std::uint32_t>(),
                    combatMode(order), order.at("reinforcement").get<std::uint64_t>(), order.at("retreat").get<std::uint64_t>()});
            }
        }
        if (const auto march = entry.find("march"); march != entry.end()) {
            player.marching = March{tileID(march->at("source")), tileID(march->at("target")),
                march->at("troops").get<std::uint32_t>(), timeAt(*march, "arrives"),
                march->at("sequence").get<std::uint64_t>(), combatMode(*march), march->at("reinforcement").get<std::uint64_t>(),
                savedTroopType(*march, "troop_type")};
        }
    }
    engagements_.clear();
    for (const auto& battle : battles) {
        const auto id = battle.at("id").get<std::uint64_t>();
        const auto attacker = battle.at("attacker").get<int>(), defender = battle.at("defender").get<int>();
        if (id == 0 || id >= nextBattleId_ || attacker < 1 || attacker > static_cast<int>(players_.size()) ||
            defender < 1 || defender > static_cast<int>(players_.size()) || attacker == defender) {
            throw std::runtime_error("invalid engagement in world snapshot");
        }
        const auto target = tileID(battle.at("target"));
        // 旧快照没有地貌字段，从战斗据点恢复场景，仍按快照记录的规则版本结算。
        const auto terrain = static_cast<battle::Terrain>(battle.value("terrain", static_cast<unsigned>(tiles_[target - 1].terrain())));
        if (terrain > battle::Terrain::ford) throw std::runtime_error("invalid saved battlefield terrain");
        const battle::Request request{battle.at("rule").get<std::uint32_t>(), battle.at("attacker_troops").get<std::uint32_t>(),
            battle.at("defender_troops").get<std::uint32_t>(), battle.at("round_seed").get<std::uint64_t>(),
            savedTroopType(battle, "attacker_type"), savedTroopType(battle, "defender_type"), terrain};
        engagements_.emplace(id, Engagement{{id, target, attacker, defender, request, battle.at("round").get<unsigned>()},
            battle.at("rounds").get<unsigned>(), timeAt(battle, "round_ends"), battle.at("seed").get<std::uint64_t>(),
            battle.at("initial_attacker").get<std::uint32_t>(), battle.at("initial_defender").get<std::uint32_t>(),
            battle.at("attacker_waiting").get<std::uint32_t>(), battle.at("defender_waiting").get<std::uint32_t>(),
            battle.at("attacker_reinforcements").get<std::uint32_t>(), battle.at("defender_reinforcements").get<std::uint32_t>(),
            battle.value("last_attacker_losses", 0u), battle.value("last_defender_losses", 0u)});
    }
    battleReports_ = data.at("reports").get<std::deque<Json>>();
    newOrder_.reset();
    return {now, legacy};
}

void World::pauseFor(Clock::duration duration) {
    logicalEpoch_ += duration;
    for (auto& player : players_) {
        if (player.account.empty()) continue;
        player.lastSeen += duration;
        player.lastReinforcement += duration;
        if (player.marching) player.marching->arrives += duration;
    }
    for (auto& [_, engagement] : engagements_) engagement.roundEnds += duration;
}

std::vector<SavedOrder> World::legacyOrders() const {
    std::vector<SavedOrder> result;
    for (const auto& player : players_) {
        if (!player.orders) continue;
        for (const auto& [id, order] : *player.orders) result.push_back(savedOrder(player.seat, id, order));
    }
    return result;
}

void World::restoreOrder(SavedOrder saved) {
    const auto player = std::ranges::find(players_, saved.seat, &Player::seat);
    if (saved.seat == 0 || player == players_.end() || saved.id.empty() || saved.id.size() > 64 ||
        saved.target < 1 || saved.target > static_cast<int>(tiles_.size()) || saved.troops > battle::maximumTroops ||
        saved.mode < static_cast<int>(CombatMode::instant) || saved.mode > static_cast<int>(CombatMode::continuous) ||
        saved.troopType > static_cast<unsigned>(battle::TroopType::cavalry)) {
        throw std::runtime_error("invalid saved world command");
    }
    player->orders->emplace(std::move(saved.id), PlacedOrder{saved.target, saved.troops, static_cast<CombatMode>(saved.mode),
        saved.reinforcement, saved.retreat, static_cast<battle::TroopType>(saved.troopType)});
}

}  // slg 命名空间
