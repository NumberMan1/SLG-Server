#pragma once

#include <atomic>
#include <future>
#include <memory>

#include <orbit/serial_queue.hpp>

#include "world.hpp"
#include "world_store.hpp"

namespace slg {

// 用 Orbit 队列独占一张地图。网络线程通过这里提交命令，队列内执行地图更新和交锋。
class WorldActor {
public:
    WorldActor(std::chrono::seconds seatIdleTimeout, std::chrono::seconds checkpointInterval, int radius, std::unique_ptr<WorldStore> store);
    ~WorldActor();

    // 事件提交后回执。等待超时的写入仍可提交，客户端须沿用原指令 ID 重试。
    std::optional<nlohmann::json> update(std::string account, nlohmann::json request);
    bool ready() const { return ready_.load() && hasLease(); }
    // 失租后的进程需要退出，重启后从已提交世界重新取得租约。
    bool leaseLost() const { return !hasLease(); }

private:
    using Clock = std::chrono::steady_clock;
    using Reply = std::optional<nlohmann::json>;
    struct Request {
        std::string account;
        nlohmann::json body;
        Clock::time_point deadline;
        std::promise<Reply> reply;
    };
    struct PendingWrite {
        std::shared_ptr<const WorldEvent> event;
        std::shared_ptr<Request> request;
        World::Result result;
        Clock::time_point recordedAt;
        std::future<bool> completion;
        Clock::time_point retryAt{};
    };
    struct Cleanup {
        std::uint64_t completed = 0, running = 0;
        Clock::time_point retryAt{};
        std::future<bool> result;
    };

    bool hasLease() const { return !store_ || Clock::now() < leaseUntil_.load(); }
    void renewLease();
    void receive(const std::shared_ptr<Request>& request);
    void advance();
    bool finishWrite();
    void record(const std::shared_ptr<Request>& request = {}, World::Result result = World::Result::ok);
    void submitWrite();
    void maybeCheckpoint();
    void maybeClean(Cleanup& cleanup, std::uint64_t target, const char* label, std::function<bool(std::uint64_t)> prune);

    World world_;
    // 恢复存档之前取得进程独有标记，避免另一写者提交的相同地图被误认作本次事务。
    const std::string writerId_;
    const std::unique_ptr<WorldStore> store_;
    const std::chrono::seconds checkpointInterval_;
    std::optional<World> committed_;
    std::uint64_t savedRevision_ = 0;
    Clock::time_point savedAt_{};
    std::uint64_t checkpointRevision_ = 0, writingCheckpoint_ = 0;
    Clock::time_point nextCheckpoint_{}, checkpointRetryAt_{};
    std::future<std::uint64_t> checkpointResult_;
    std::uint64_t coveredEvents_ = 0, retiredOrdersBefore_ = 0;
    bool repairCheckpoint_ = false;
    Cleanup eventCleanup_, orderCleanup_;
    std::optional<PendingWrite> pending_;
    std::deque<std::shared_ptr<Request>> waiting_;
    std::atomic<bool> ready_{true};
    std::atomic<Clock::time_point> leaseUntil_{};
    bool fenced_ = false;
    orbit::SerialQueue queue_;
    orbit::SerialQueue writer_;
    orbit::SerialQueue checkpointer_;
    orbit::SerialQueue cleaner_;
    orbit::SerialQueue leaseWorker_;
};

}  // slg 命名空间
