#include "world_store.hpp"

#include <algorithm>
#include <array>
#include <charconv>
#include <cmath>
#include <cstdlib>
#include <iostream>
#include <limits>
#include <stdexcept>
#include <string_view>
#include <utility>

#include <mysql.h>

#include <orbit/tcp_endpoint.hpp>

#include "world.hpp"

namespace slg {
namespace {

std::string_view setting(const char* name) {
    if (const auto value = std::getenv(name)) return value;
    return {};
}

// 短二进制键用十六进制字面量，世界存档本体通过 BLOB 参数传输。
std::string binaryLiteral(std::string_view value) {
    constexpr std::string_view hex = "0123456789abcdef";
    std::string literal = "X'";
    literal.reserve(value.size() * 2 + 3);
    for (const unsigned char byte : value) {
        literal.push_back(hex[byte >> 4]);
        literal.push_back(hex[byte & 15]);
    }
    literal.push_back('\'');
    return literal;
}

template <typename Integer>
Integer column(const char* text, unsigned long length) {
    Integer value = 0;
    const auto end = text + length;
    const auto parsed = std::from_chars(text, end, value);
    if (parsed.ec != std::errc{} || parsed.ptr != end) throw std::runtime_error("invalid world storage number");
    return value;
}

// Connector/C 以秒接收超时。兼容 Go DSN 的时长写法，正数不足一秒向上取整。
unsigned timeoutSeconds(std::string_view value) {
    if (value.starts_with('+')) value.remove_prefix(1);
    if (value == "0") return 0;
    double seconds = 0;
    const std::array units{std::pair{std::string_view("ns"), 1e-9}, std::pair{std::string_view("us"), 1e-6},
        std::pair{std::string_view("µs"), 1e-6}, std::pair{std::string_view("μs"), 1e-6},
        std::pair{std::string_view("ms"), 1e-3}, std::pair{std::string_view("s"), 1.0},
        std::pair{std::string_view("m"), 60.0}, std::pair{std::string_view("h"), 3600.0}};
    constexpr auto invalid = "invalid game MySQL timeout duration";
    if (value.empty()) throw std::invalid_argument(invalid);
    while (!value.empty()) {
        const auto unitAt = value.find_first_not_of("0123456789.");
        if (unitAt == value.npos) throw std::invalid_argument(invalid);
        const auto end = value.data() + unitAt;
        double number = 0;
        const auto parsed = std::from_chars(value.data(), end, number);
        if (parsed.ec != std::errc{} || parsed.ptr != end) throw std::invalid_argument(invalid);
        value.remove_prefix(unitAt);
        bool found = false;
        for (const auto& [unit, multiplier] : units) {
            if (value.starts_with(unit)) {
                seconds += number * multiplier;
                value.remove_prefix(unit.size());
                found = true;
                break;
            }
        }
        if (!found || seconds > std::numeric_limits<unsigned>::max()) throw std::invalid_argument(invalid);
    }
    return static_cast<unsigned>(std::ceil(seconds));
}

std::string databaseName(std::string_view value) {
    std::string decoded;
    for (std::size_t i = 0; i < value.size(); ++i) {
        if (value[i] != '%') {
            decoded.push_back(value[i]);
            continue;
        }
        if (i + 2 >= value.size()) throw std::invalid_argument("invalid escaped database name");
        const auto begin = value.data() + i + 1;
        unsigned byte = 0;
        const auto parsed = std::from_chars(begin, begin + 2, byte, 16);
        if (parsed.ec != std::errc{} || parsed.ptr != begin + 2 || byte == 0) {
            throw std::invalid_argument("invalid escaped database name");
        }
        decoded.push_back(static_cast<char>(byte));
        i += 2;
    }
    return decoded;
}

}  // 匿名命名空间

class WorldStore::Connection {
public:
    enum class Blob { initial, upgrade, event, checkpoint, registerChecksum };
    explicit Connection(const Config& config) : config_(config) {}
    bool connected() const { return handle_ != nullptr; }
    ~Connection() {
        reset();
        if (initialized_) mysql_thread_end();
    }

    MYSQL* get() {
        if (handle_) return handle_;
        if (!initialized_) {
            if (mysql_thread_init() != 0) throw std::runtime_error("initialize MySQL client thread");
            initialized_ = true;
        }
        handle_ = mysql_init(nullptr);
        if (!handle_) throw std::runtime_error("initialize MySQL connection");
        const unsigned protocol = MYSQL_PROTOCOL_TCP;
        mysql_options(handle_, MYSQL_OPT_CONNECT_TIMEOUT, &config_.connectTimeout);
        mysql_options(handle_, MYSQL_OPT_READ_TIMEOUT, &config_.readTimeout);
        mysql_options(handle_, MYSQL_OPT_WRITE_TIMEOUT, &config_.writeTimeout);
        mysql_options(handle_, MYSQL_OPT_PROTOCOL, &protocol);
        mysql_options(handle_, MYSQL_SET_CHARSET_NAME, "utf8mb4");
        mysql_options(handle_, MYSQL_OPT_SSL_ENFORCE, &config_.tls);
        mysql_options(handle_, MYSQL_OPT_SSL_VERIFY_SERVER_CERT, &config_.tls);
        if (!mysql_real_connect(handle_, config_.host.c_str(), config_.user.c_str(), config_.password.c_str(),
                                config_.schema.c_str(), config_.port, nullptr, 0)) {
            throw failure("connect");
        }
        if (mysql_autocommit(handle_, true) != 0) throw failure("enable autocommit");
        return handle_;
    }

    MYSQL* query(const std::string& sql) {
        auto* handle = get();
        if (mysql_real_query(handle, sql.data(), static_cast<unsigned long>(sql.size())) != 0) {
            throw failure("query");
        }
        return handle;
    }

    bool write(Blob operation, const std::string& world, std::uint64_t revision, const std::string& payload) {
        auto* handle = get();
        constexpr std::array<std::string_view, 5> statements{
            "INSERT INTO slg_world_snapshots (payload,world_id,revision) VALUES (?,?,?)",
            "UPDATE slg_world_snapshots SET payload=? WHERE world_id=? AND revision=?",
            "INSERT INTO slg_world_events (payload,world_id,revision) VALUES (?,?,?)",
            "UPDATE slg_world_snapshots SET payload=?,revision=? WHERE world_id=? AND revision<=?",
            "INSERT IGNORE INTO slg_world_snapshot_checksums (digest,world_id,revision) VALUES (UNHEX(SHA2(?,256)),?,?)",
        };
        const auto index = static_cast<std::size_t>(operation);
        auto*& statement = prepared_[index];
        if (!statement) {
            statement = mysql_stmt_init(handle);
            if (!statement) throw failure("allocate world statement");
            const auto sql = statements[index];
            if (mysql_stmt_prepare(statement, sql.data(), static_cast<unsigned long>(sql.size())) != 0) {
                throw statementFailure(statement, "prepare world write");
            }
        }
        MYSQL_BIND blob{}, key{}, number{};
        blob.buffer_type = MYSQL_TYPE_LONG_BLOB;
        blob.buffer = const_cast<char*>(payload.data());
        blob.buffer_length = static_cast<unsigned long>(payload.size());
        key.buffer_type = MYSQL_TYPE_BLOB;
        key.buffer = const_cast<char*>(world.data());
        key.buffer_length = static_cast<unsigned long>(world.size());
        number.buffer_type = MYSQL_TYPE_LONGLONG;
        number.buffer = &revision;
        number.is_unsigned = true;
        auto parameters = operation == Blob::checkpoint
            ? std::array{blob, number, key, number} : std::array{blob, key, number, MYSQL_BIND{}};
        if (mysql_stmt_bind_param(statement, parameters.data()) != 0 || mysql_stmt_execute(statement) != 0) {
            throw statementFailure(statement, "write world data");
        }
        return mysql_stmt_affected_rows(statement) == 1;
    }

    std::runtime_error failure(const char* operation) {
        const auto code = mysql_errno(handle_);
        const std::string message = mysql_error(handle_);
        reset();
        return std::runtime_error("world MySQL " + std::string(operation) + " (" + std::to_string(code) + "): " + message);
    }

    void reset() {
        for (auto*& statement : prepared_) {
            if (statement) mysql_stmt_close(std::exchange(statement, nullptr));
        }
        if (handle_) mysql_close(std::exchange(handle_, nullptr));
    }

private:
    std::runtime_error statementFailure(MYSQL_STMT* statement, const char* operation) {
        const auto code = mysql_stmt_errno(statement);
        const std::string message = mysql_stmt_error(statement);
        reset();
        return std::runtime_error("world MySQL " + std::string(operation) + " (" + std::to_string(code) + "): " + message);
    }

    Config config_;
    MYSQL* handle_ = nullptr;
    std::array<MYSQL_STMT*, 5> prepared_{};
    bool initialized_ = false;
};

WorldStore::WorldStore(Config config, std::string world)
    : config_(std::move(config)), world_(std::move(world)), key_(binaryLiteral(world_)) {}

std::unique_ptr<WorldStore> WorldStore::fromEnvironment() {
    auto dsn = setting("SLG_GAME_MYSQL_DSN");
    if (dsn.empty()) dsn = setting("SLG_MYSQL_DSN");
    auto world = setting("SLG_WORLD_ID");
    if (dsn.empty() && world.empty()) return nullptr;
    if (world.size() > 64) throw std::invalid_argument("SLG_WORLD_ID exceeds 64 bytes");
    if (world.empty()) world = "frontier";

    // 沿用登录服务的 TCP DSN 格式，游戏服允许单独覆盖凭据或调试地址。
    const auto transport = dsn.rfind("@tcp(");
    const auto close = transport == dsn.npos ? dsn.npos : dsn.find(")/", transport + 5);
    if (close == dsn.npos || transport == 0) {
        throw std::invalid_argument("game MySQL DSN must use user[:password]@tcp(host:port)/database");
    }
    auto address = setting("SLG_MYSQL_ADDRESS");
    if (address.empty()) address = dsn.substr(transport + 5, close - transport - 5);
    auto endpointText = std::string(address);
    if (endpointText.empty()) endpointText = "127.0.0.1";
    if (endpointText.find(':') == endpointText.npos || endpointText.ends_with(']')) endpointText += ":3306";
    const auto endpoint = orbit::parse_tcp_endpoint(endpointText, orbit::TcpEndpointUse::connect);
    const auto credentials = dsn.substr(0, transport);
    const auto colon = credentials.find(':');
    auto databaseAndOptions = dsn.substr(close + 2);
    const auto question = databaseAndOptions.find('?');
    const auto database = databaseAndOptions.substr(0, question);
    if (database.empty() || colon == 0) throw std::invalid_argument("game MySQL DSN requires a user and database");
    Config config{endpoint.host, std::string(credentials.substr(0, colon)),
                  colon == credentials.npos ? "" : std::string(credentials.substr(colon + 1)),
                  databaseName(database), static_cast<unsigned>(endpoint.port), false};
    if (question != databaseAndOptions.npos) {
        auto options = databaseAndOptions.substr(question + 1);
        while (!options.empty()) {
            const auto ampersand = options.find('&');
            const auto option = options.substr(0, ampersand);
            if (option == "tls=true") config.tls = true;
            else if (option == "tls=false") config.tls = false;
            else if (option.starts_with("timeout=")) config.connectTimeout = timeoutSeconds(option.substr(8));
            else if (option.starts_with("readTimeout=")) config.readTimeout = timeoutSeconds(option.substr(12));
            else if (option.starts_with("writeTimeout=")) config.writeTimeout = timeoutSeconds(option.substr(13));
            else if (!option.starts_with("parseTime=") && !option.starts_with("loc=") && option != "charset=utf8mb4") {
                throw std::invalid_argument("unsupported game MySQL DSN option, use SLG_GAME_MYSQL_DSN to override");
            }
            if (ampersand == options.npos) break;
            options.remove_prefix(ampersand + 1);
        }
    }
    return std::unique_ptr<WorldStore>(new WorldStore(std::move(config), std::string(world)));
}

bool WorldStore::acquireLease(const std::string& writer) const {
    static const auto initialization = mysql_library_init(0, nullptr, nullptr);
    if (initialization != 0) throw std::runtime_error("initialize MySQL client library");
    Connection connection(config_);
    const auto token = binaryLiteral(writer);
    const auto expires = "(UTC_TIMESTAMP(6) + INTERVAL " + std::to_string(leaseDuration.count()) + " SECOND)";
    auto* handle = connection.query("INSERT IGNORE INTO slg_world_leases (world_id,writer,expires_at) VALUES (" +
        key_ + "," + token + "," + expires + ")");
    if (mysql_affected_rows(handle) == 1) return true;
    handle = connection.query("UPDATE slg_world_leases SET writer=" + token + ",expires_at=" + expires +
        " WHERE world_id=" + key_ + " AND expires_at<=UTC_TIMESTAMP(6)");
    return mysql_affected_rows(handle) == 1;
}

bool WorldStore::renewLease(const std::string& writer) const {
    thread_local Connection connection(config_);
    auto* handle = connection.query("UPDATE slg_world_leases SET expires_at=UTC_TIMESTAMP(6) + INTERVAL " +
        std::to_string(leaseDuration.count()) + " SECOND WHERE world_id=" + key_ + " AND writer=" + binaryLiteral(writer) +
        " AND expires_at>UTC_TIMESTAMP(6)");
    return mysql_affected_rows(handle) == 1;
}

bool WorldStore::lockLease(Connection& connection, const std::string& writer) const {
    auto* handle = connection.query("SELECT 1 FROM slg_world_leases WHERE world_id=" + key_ +
        " AND writer=" + binaryLiteral(writer) + " AND expires_at>UTC_TIMESTAMP(6) FOR UPDATE");
    std::unique_ptr<MYSQL_RES, decltype(&mysql_free_result)> result(mysql_store_result(handle), mysql_free_result);
    if (!result) throw connection.failure("check world lease");
    return mysql_fetch_row(result.get()) != nullptr;
}

std::optional<WorldStore::SavedWorld> WorldStore::load(World& world) const {
    Connection connection(config_);
    // 恢复只读取同一 MySQL 一致性视图中的检查点、后续事件和指令记录。
    connection.query("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ");
    connection.query("START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY");
    auto snapshot = loadSnapshot(connection);
    const auto head = loadRevision(connection, "slg_world_streams");
    const auto previous = loadRevision(connection, "slg_world_previous_snapshots");
    bool recoveredFromPrevious = false;
    if (!snapshot) {
        if (!head) return std::nullopt;
        if (previous) snapshot = loadSnapshot(connection, true);
        if (!snapshot) throw std::runtime_error("world event stream has no checkpoint");
        recoveredFromPrevious = true;
    }
    const auto restore = [&](const Snapshot& base) {
        if (base.checksumMatches == false) throw std::runtime_error("world snapshot checksum does not match");
        if (head && *head < base.revision) throw std::runtime_error("world checkpoint is newer than its event stream");
        std::function<void(const World::ApplyEvent&)> readEvents;
        if (head && *head > base.revision) {
            readEvents = [&](const auto& apply) { loadEvents(connection, base.revision, *head, apply); };
        }
        return world.restore(base.payload, readEvents, head.has_value());
    };
    World::RestoreState restored;
    try {
        restored = restore(*snapshot);
    } catch (const WorldLayoutError&) {
        throw;
    } catch (const std::exception& error) {
        if (!connection.connected() || recoveredFromPrevious || !previous || !head || *previous >= snapshot->revision) throw;
        std::cerr << "world checkpoint " << snapshot->revision << ": " << error.what() << '\n';
        snapshot = loadSnapshot(connection, true);
        if (!snapshot) throw;
        restored = restore(*snapshot);
        recoveredFromPrevious = true;
    }
    auto legacyOrders = restored.legacyOrders ? world.legacyOrders() : std::vector<SavedOrder>{};
    loadOrders(connection, world);
    const auto verifiedPrevious = loadVerifiedRevision(connection, true);
    connection.query("COMMIT");
    if (head && !snapshot->checksumMatches.has_value()) {
        // 首次读取旧格式时，只为已通过回放校验的快照补登记校验和。
        connection.write(Connection::Blob::registerChecksum, world_, snapshot->revision, snapshot->payload);
    }
    const auto readyAt = World::Clock::now();
    world.pauseFor(readyAt - restored.at);
    const auto pruneThrough = recoveredFromPrevious ? snapshot->revision : verifiedPrevious.value_or(0);
    return SavedWorld{snapshot->revision, head, pruneThrough, recoveredFromPrevious, readyAt, std::move(legacyOrders)};
}

std::optional<WorldStore::Snapshot> WorldStore::loadSnapshot(Connection& connection, bool previous) const {
    const std::string table = previous ? "slg_world_previous_snapshots" : "slg_world_snapshots";
    auto* handle = connection.query("SELECT s.revision,s.payload,c.digest=UNHEX(SHA2(s.payload,256)) FROM " +
        table + " s LEFT JOIN slg_world_snapshot_checksums c USING (world_id,revision) WHERE s.world_id=" + key_);
    std::unique_ptr<MYSQL_RES, decltype(&mysql_free_result)> result(mysql_store_result(handle), mysql_free_result);
    if (!result) throw connection.failure("load snapshot");
    const auto row = mysql_fetch_row(result.get());
    if (!row) return std::nullopt;
    const auto lengths = mysql_fetch_lengths(result.get());
    const auto revision = column<std::uint64_t>(row[0], lengths[0]);
    const auto matches = row[2] ? std::optional{row[2][0] == '1'} : std::nullopt;
    return Snapshot{revision, std::string(row[1], lengths[1]), matches};
}

void WorldStore::saveSnapshotChecksum(Connection& connection, std::uint64_t revision) const {
    // 和快照同事务，用刚写入的列计算摘要，避免从游戏服再次发送完整正文。
    auto* handle = connection.query("REPLACE INTO slg_world_snapshot_checksums (world_id,revision,digest) "
        "SELECT world_id,revision,UNHEX(SHA2(payload,256)) FROM slg_world_snapshots WHERE world_id=" + key_ +
        " AND revision=" + std::to_string(revision));
    if (mysql_affected_rows(handle) == 0) throw std::runtime_error("world snapshot checksum was not installed");
}

std::optional<std::uint64_t> WorldStore::loadVerifiedRevision(Connection& connection, bool previous) const {
    const std::string table = previous ? "slg_world_previous_snapshots" : "slg_world_snapshots";
    auto* handle = connection.query("SELECT s.revision FROM " + table +
        " s JOIN slg_world_snapshot_checksums c USING (world_id,revision) WHERE s.world_id=" + key_ +
        " AND c.digest=UNHEX(SHA2(s.payload,256))");
    std::unique_ptr<MYSQL_RES, decltype(&mysql_free_result)> result(mysql_store_result(handle), mysql_free_result);
    if (!result) throw connection.failure("verify world snapshot");
    if (const auto row = mysql_fetch_row(result.get())) {
        return column<std::uint64_t>(row[0], mysql_fetch_lengths(result.get())[0]);
    }
    return std::nullopt;
}

std::optional<std::uint64_t> WorldStore::loadRevision(Connection& connection, std::string_view table, bool lock) const {
    auto* handle = connection.query("SELECT revision FROM " + std::string(table) + " WHERE world_id=" + key_ +
        (lock ? " FOR UPDATE" : ""));
    std::unique_ptr<MYSQL_RES, decltype(&mysql_free_result)> result(mysql_store_result(handle), mysql_free_result);
    if (!result) throw connection.failure("load stream revision");
    if (const auto row = mysql_fetch_row(result.get())) {
        return column<std::uint64_t>(row[0], mysql_fetch_lengths(result.get())[0]);
    }
    return std::nullopt;
}

void WorldStore::loadEvents(Connection& connection, std::uint64_t after, std::uint64_t head,
                            const std::function<void(std::string_view)>& apply) const {
    auto* handle = connection.query("SELECT revision,payload FROM slg_world_events WHERE world_id=" + key_ +
        " AND revision>" + std::to_string(after) + " ORDER BY revision");
    // 顺序消费结果并立即应用，每次只保留当前事件和正在恢复的地图。
    std::unique_ptr<MYSQL_RES, decltype(&mysql_free_result)> result(mysql_use_result(handle), mysql_free_result);
    if (!result) throw connection.failure("load world events");
    while (const auto row = mysql_fetch_row(result.get())) {
        const auto lengths = mysql_fetch_lengths(result.get());
        if (after == head || column<std::uint64_t>(row[0], lengths[0]) != ++after) {
            throw std::runtime_error("world event stream is out of order");
        }
        apply(std::string_view(row[1], lengths[1]));
    }
    if (mysql_errno(handle) != 0) {
        result.reset();
        throw connection.failure("read world events");
    }
    if (after != head) throw std::runtime_error("world event stream has missing entries");
}

void WorldStore::loadOrders(Connection& connection, World& world) const {
    const auto seats = world.seats();
    auto* handle = connection.query("SELECT seat_revision,command_id,target_tile_id,troops,combat_mode,reinforcement,retreat "
        "FROM slg_world_orders WHERE world_id=" + key_ + " AND seat_revision IN (" + std::to_string(seats[0]) + "," +
        std::to_string(seats[1]) + ")");
    std::unique_ptr<MYSQL_RES, decltype(&mysql_free_result)> result(mysql_use_result(handle), mysql_free_result);
    if (!result) throw connection.failure("load orders");
    while (const auto row = mysql_fetch_row(result.get())) {
        const auto lengths = mysql_fetch_lengths(result.get());
        world.restoreOrder({column<std::uint64_t>(row[0], lengths[0]), std::string(row[1], lengths[1]),
            column<int>(row[2], lengths[2]), column<std::uint32_t>(row[3], lengths[3]), column<int>(row[4], lengths[4]),
            column<std::uint64_t>(row[5], lengths[5]), column<std::uint64_t>(row[6], lengths[6])});
    }
    if (mysql_errno(handle) != 0) {
        result.reset();
        throw connection.failure("read world orders");
    }
}

void WorldStore::insertOrders(Connection& connection, const std::vector<SavedOrder>& orders) const {
    // 常规提交只追加一条成功指令。旧存档迁移时分批发送历史记录以限制报文大小。
    for (std::size_t i = 0; i < orders.size();) {
        std::string sql = "INSERT INTO slg_world_orders (world_id,seat_revision,command_id,target_tile_id,troops,combat_mode,reinforcement,retreat) VALUES ";
        const auto end = std::min(i + 128, orders.size());
        for (; i < end; ++i) {
            const auto& order = orders[i];
            sql += "(" + key_ + "," + std::to_string(order.seat) + "," + binaryLiteral(order.id) + "," +
                std::to_string(order.target) + "," + std::to_string(order.troops) + "," + std::to_string(order.mode) + "," +
                std::to_string(order.reinforcement) + "," + std::to_string(order.retreat) + (i + 1 < end ? ")," : ")");
        }
        connection.query(sql);
    }
}

bool WorldStore::initialize(std::optional<std::uint64_t> previous, const std::string& snapshot,
                            const std::vector<SavedOrder>& orders) const {
    Connection connection(config_);
    connection.query("START TRANSACTION");
    const auto revision = previous.value_or(0);
    auto* handle = connection.query("INSERT INTO slg_world_streams (world_id,revision) VALUES (" + key_ + "," +
        std::to_string(revision) + ") ON DUPLICATE KEY UPDATE revision=revision");
    if (mysql_affected_rows(handle) != 1 || !connection.write(previous ? Connection::Blob::upgrade : Connection::Blob::initial,
            world_, revision, snapshot)) return false;
    saveSnapshotChecksum(connection, revision);
    insertOrders(connection, orders);
    connection.query("COMMIT");
    return true;
}

bool WorldStore::append(std::uint64_t expectedRevision, const WorldEvent& event, const std::string& writer) const {
    // 写线程复用 MySQL 会话。事件、流版本和去重表在同一事务提交。
    thread_local Connection connection(config_);
    const auto next = expectedRevision + 1;
    try {
        connection.query("START TRANSACTION");
        auto* handle = connection.query("UPDATE slg_world_streams SET revision=revision+1 WHERE world_id=" + key_ +
            " AND revision=" + std::to_string(expectedRevision));
        const bool advanced = mysql_affected_rows(handle) == 1;
        if (advanced) {
            if (!connection.write(Connection::Blob::event, world_, next, event.payload)) {
                throw std::runtime_error("world event was not appended");
            }
            insertOrders(connection, event.orders);
        }
        // 流头或回执行等待期间允许续约。租约只在提交前锁定，失租会回滚整笔事件。
        if (!lockLease(connection, writer)) {
            connection.query("ROLLBACK");
            return false;
        }
        if (advanced) {
            connection.query("COMMIT");
            return true;
        }
        connection.query("ROLLBACK");
        // 提交回执可能丢失。只有流头仍是同一序号，且该事件内容匹配，才能确认本次写入。
        handle = connection.query("SELECT e.payload FROM slg_world_events e JOIN slg_world_streams s "
            "ON s.world_id=e.world_id AND s.revision=e.revision WHERE e.world_id=" + key_ + " AND e.revision=" + std::to_string(next));
        std::unique_ptr<MYSQL_RES, decltype(&mysql_free_result)> result(mysql_store_result(handle), mysql_free_result);
        if (!result) throw connection.failure("confirm world event");
        const auto row = mysql_fetch_row(result.get());
        return row && std::string_view(row[0], mysql_fetch_lengths(result.get())[0]) == event.payload;
    } catch (...) {
        // 遇到断线或事务内报错时丢弃连接，避免重试隐式提交前一事务。
        connection.reset();
        throw;
    }
}

WorldStore::Connection& WorldStore::maintenanceConnection() const {
    thread_local Connection connection(config_);
    return connection;
}

WorldStore::Connection& WorldStore::cleanupConnection() const {
    thread_local Connection connection(config_);
    if (!connection.connected()) {
        connection.query("SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED");
    }
    return connection;
}

std::uint64_t WorldStore::checkpoint(std::uint64_t revision, const std::string& snapshot, bool repairing) const {
    auto& connection = maintenanceConnection();
    try {
        connection.query("START TRANSACTION");
        const auto current = loadRevision(connection, "slg_world_snapshots", true);
        auto installedRevision = current.value_or(revision);
        if (!current) {
            if (!repairing || !connection.write(Connection::Blob::initial, world_, revision, snapshot)) {
                throw std::runtime_error("world checkpoint is missing");
            }
            saveSnapshotChecksum(connection, revision);
        } else if (*current < revision || (repairing && *current == revision)) {
            // 只有内容仍匹配原校验和的快照才可以替换已有回退点。
            if (!repairing && loadVerifiedRevision(connection, false)) {
                connection.query("DELETE FROM slg_world_previous_snapshots WHERE world_id=" + key_);
                connection.query("INSERT INTO slg_world_previous_snapshots SELECT * FROM slg_world_snapshots WHERE world_id=" + key_);
            }
            if (!connection.write(Connection::Blob::checkpoint, world_, revision, snapshot) && *current != revision) {
                throw std::runtime_error("world checkpoint was not installed");
            }
            saveSnapshotChecksum(connection, revision);
            installedRevision = revision;
        }
        const auto previous = loadVerifiedRevision(connection, true);
        const auto previousRevision = previous ? previous : loadRevision(connection, "slg_world_previous_snapshots");
        auto keep = std::to_string(installedRevision);
        if (previousRevision) keep += "," + std::to_string(*previousRevision);
        connection.query("DELETE FROM slg_world_snapshot_checksums WHERE world_id=" + key_ + " AND revision NOT IN (" + keep + ")");
        connection.query("COMMIT");
        return previous.value_or(0);
    } catch (...) {
        connection.reset();
        throw;
    }
}

bool WorldStore::pruneEvents(std::uint64_t coveredThrough) const {
    auto& connection = cleanupConnection();
    constexpr unsigned batchSize = 256;
    auto* handle = connection.query("DELETE FROM slg_world_events WHERE world_id=" + key_ + " AND revision<=" +
        std::to_string(coveredThrough) + " ORDER BY revision LIMIT " + std::to_string(batchSize));
    return mysql_affected_rows(handle) < batchSize;
}

bool WorldStore::pruneOrders(std::uint64_t before, const std::array<std::uint64_t, 2>& active) const {
    auto& connection = cleanupConnection();
    constexpr unsigned batchSize = 256;
    auto* handle = connection.query("DELETE FROM slg_world_orders WHERE world_id=" + key_ + " AND seat_revision<" +
        std::to_string(before) + " AND seat_revision NOT IN (" + std::to_string(active[0]) + "," +
        std::to_string(active[1]) + ") ORDER BY seat_revision,command_id LIMIT " + std::to_string(batchSize));
    return mysql_affected_rows(handle) < batchSize;
}

}  // slg 命名空间
