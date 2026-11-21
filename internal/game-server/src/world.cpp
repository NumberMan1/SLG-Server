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
        index = join(account);
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
    if (index >= 0 && (!request.contains("known_instance") || request["known_instance"] != instance_ ||
                      !request.contains("known_revision") || knownRevision(request["known_revision"]) != revision_)) {
        reply["snapshot"] = snapshot(index, now, unixMs);
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

int World::join(const std::string& account) {
    const int index = findPlayer("");
    if (index < 0) {
        return -1;
    }
    players_[index].account = account;
    for (auto& tile : tiles_) {
        if (tile.home == index + 1) {
            tile.owner = index + 1;
            break;
        }
    }
    ++revision_;
    return index;
}

World::Result World::placeOrder(int index, const Json& order, Clock::time_point now) {
    if (!order.is_object() || !order.contains("command_id") || !order["command_id"].is_string() ||
        !order.contains("target_tile_id") || !order["target_tile_id"].is_number_unsigned()) {
        return Result::invalid_order;
    }
    const auto& id = order["command_id"].get_ref<const std::string&>();
    const auto targetId = order["target_tile_id"].get<std::uint64_t>();
    if (id.empty() || id.size() > 64 || targetId == 0 || targetId > tiles_.size()) {
        return Result::invalid_order;
    }

    auto& player = players_[index];
    // 成功指令保留 ID，跨连接重试也只会生成一支部队。
    if (const auto prior = player.orders.find(id); prior != player.orders.end()) {
        return prior->second == static_cast<int>(targetId) ? Result::ok : Result::invalid_order;
    }
    if (player.marching) {
        return Result::already_marching;
    }
    const auto& target = tiles_[targetId - 1];
    if (target.owner != 0 || target.home != 0) {
        return Result::invalid_target;
    }
    // 发令时预留中立目标，避免两个玩家同时占领同一地块。
    for (const auto& other : players_) {
        if (other.marching && other.marching->target == target.id) {
            return Result::reserved;
        }
    }
    for (const auto& source : tiles_) {
        const int q = source.q - target.q, r = source.r - target.r;
        if (source.owner == index + 1 && std::max({std::abs(q), std::abs(r), std::abs(q + r)}) == 1) {
            player.orders.emplace(id, target.id);
            player.marching = March{source.id, target.id, now + std::chrono::seconds(3)};
            ++revision_;
            return Result::ok;
        }
    }
    return Result::invalid_target;
}

void World::expireSeats(Clock::time_point now) {
    bool recycled = false;
    for (int index = 0; index < static_cast<int>(players_.size()); ++index) {
        auto& player = players_[index];
        if (player.account.empty() || now - player.lastSeen < seatIdleTimeout_) {
            continue;
        }
        // 回收该玩家的地块、行军预留和指令记录，其他玩家的进度继续保留。
        player = Player{};
        for (auto& tile : tiles_) {
            if (tile.owner == index + 1) {
                tile.owner = 0;
            }
        }
        recycled = true;
    }
    if (recycled) {
        // 名额重新分配后更换世界实例，旧实例的指令无法在新归属下重新提交。
        instance_ = instanceId();
        ++revision_;
    }
}

void World::advance(Clock::time_point now) {
    for (int index = 0; index < static_cast<int>(players_.size()); ++index) {
        auto& active = players_[index].marching;
        if (active && now >= active->arrives) {
            tiles_[active->target - 1].owner = index + 1;
            active.reset();
            ++revision_;
        }
    }
}

Json World::snapshot(int index, Clock::time_point now, std::int64_t unixMs) const {
    Json tiles = Json::array(), marches = Json::array();
    for (const auto& tile : tiles_) {
        tiles.push_back({{"id", tile.id}, {"q", tile.q}, {"r", tile.r}, {"owner", tile.owner}, {"home", tile.home}});
    }
    for (int i = 0; i < static_cast<int>(players_.size()); ++i) {
        if (const auto& active = players_[i].marching) {
            auto remaining = std::chrono::duration_cast<std::chrono::milliseconds>(active->arrives - now).count();
            marches.push_back({{"player", i + 1}, {"source_tile_id", active->source},
                               {"target_tile_id", active->target}, {"arrives_at_ms", unixMs + remaining}});
        }
    }
    return {{"instance", instance_}, {"revision", revision_}, {"your_player", index + 1},
            {"tiles", std::move(tiles)}, {"marches", std::move(marches)}};
}

}  // slg 命名空间
