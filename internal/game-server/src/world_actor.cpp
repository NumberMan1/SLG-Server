#include "world_actor.hpp"

#include <future>
#include <iostream>
#include <memory>
#include <stdexcept>
#include <thread>
#include <utility>

namespace slg {
namespace {

constexpr std::uint64_t checkpointEventLimit = 128;
constexpr auto replenishmentSaveInterval = std::chrono::seconds(5);
constexpr auto leaseSafetyMargin = std::chrono::milliseconds(100);

}  // 匿名命名空间

WorldActor::WorldActor(std::chrono::seconds seatIdleTimeout, std::chrono::seconds checkpointInterval, int radius,
                       std::unique_ptr<WorldStore> store)
    : world_(seatIdleTimeout, radius), writerId_(world_.instance()), store_(std::move(store)), checkpointInterval_(checkpointInterval), queue_(64, {
          .interval = std::chrono::milliseconds(50),
          .update = [this](auto) { advance(); },
          .on_error = [](std::exception_ptr error) {
              try {
                  std::rethrow_exception(error);
              } catch (const std::exception& exception) {
                  std::cerr << "world update: " << exception.what() << '\n';
              }
          },
      }), writer_(1), checkpointer_(1), cleaner_(2), leaseWorker_(1, {
          .interval = std::chrono::milliseconds(500),
          .update = [this](auto) { renewLease(); },
      }) {
    if (store_) {
        // 在读取恢复视图前取得租约，避免两个进程同时以同一存档对外服务。
        const auto deadline = Clock::now() + std::chrono::seconds(3);
        do {
            const auto started = Clock::now();
            if (store_->acquireLease(writerId_)) {
                leaseUntil_ = started + WorldStore::leaseDuration - leaseSafetyMargin;
                break;
            }
            std::this_thread::sleep_for(std::chrono::milliseconds(50));
        } while (Clock::now() < deadline);
        if (!hasLease()) throw std::runtime_error("world already has an active writer or acquiring the lease timed out");
        if (leaseWorker_.start() != orbit::QueueStartResult::started) {
            throw std::runtime_error("game-server failed to start the world lease worker");
        }
        const auto saved = store_->load(world_);
        savedAt_ = saved ? saved->at : Clock::now();
        if (saved) {
            checkpointRevision_ = saved->checkpoint;
            savedRevision_ = saved->head.value_or(checkpointRevision_);
            coveredEvents_ = saved->pruneThrough;
            repairCheckpoint_ = saved->recoveredFromPrevious;
            retiredOrdersBefore_ = world_.stateVersion();
        }
        // 旧快照作为事件流的起点，初始化和内嵌指令迁移在同一事务提交。
        if (!saved || !saved->head) {
            const std::vector<SavedOrder> empty;
            if (!store_->initialize(saved ? std::optional{checkpointRevision_} : std::nullopt, world_.save(savedAt_),
                    saved ? saved->legacyOrders : empty)) {
                throw std::runtime_error("world changed during event stream initialization");
            }
        }
        if (!hasLease()) throw std::runtime_error("world lease expired during recovery");
        committed_.emplace(world_);
        nextCheckpoint_ = repairCheckpoint_ ? savedAt_ : savedAt_ + checkpointInterval_;
        if (writer_.start() != orbit::QueueStartResult::started || checkpointer_.start() != orbit::QueueStartResult::started ||
            cleaner_.start() != orbit::QueueStartResult::started) {
            throw std::runtime_error("game-server failed to start world storage workers");
        }
    }
    if (queue_.start() != orbit::QueueStartResult::started) {
        throw std::runtime_error("game-server failed to start world mailbox");
    }
}

WorldActor::~WorldActor() {
    // 写结果可能回投地图队列，所有后台工作结束前保留队列和存储对象。
    queue_.join();
    writer_.join();
    checkpointer_.join();
    cleaner_.join();
    leaseWorker_.join();
}

void WorldActor::renewLease() {
    const auto started = Clock::now();
    auto previous = leaseUntil_.load();
    if (started >= previous) return;
    try {
        // 用请求发出时间计算本地截止，网络延迟不会让本地授权长过数据库租约。
        const auto expires = store_->renewLease(writerId_) ? started + WorldStore::leaseDuration - leaseSafetyMargin : Clock::time_point{};
        leaseUntil_.compare_exchange_strong(previous, expires);
    } catch (const std::exception& error) {
        std::cerr << "world lease renewal: " << error.what() << '\n';
    }
}

WorldActor::Reply WorldActor::update(std::string account, nlohmann::json body) {
    const auto deadline = Clock::now() + std::chrono::milliseconds(2500);
    auto request = std::make_shared<Request>(Request{std::move(account), std::move(body), deadline, {}});
    auto response = request->reply.get_future();
    if (queue_.post([this, request] { receive(request); }) != orbit::QueuePostResult::accepted ||
        response.wait_until(deadline) != std::future_status::ready || !hasLease()) {
        return std::nullopt;
    }
    return response.get();
}

void WorldActor::receive(const std::shared_ptr<Request>& request) {
    if (Clock::now() >= request->deadline) {
        request->reply.set_value(std::nullopt);
        return;
    }
    if (!store_) {
        const auto result = world_.update(request->account, request->body);
        request->reply.set_value(world_.read(request->account, request->body, result));
        world_.clearChanges();
        return;
    }
    // 已失去写入权的进程不能再把自己的已提交状态作为当前地图返回。
    if (fenced_ || !hasLease()) {
        request->reply.set_value(std::nullopt);
        return;
    }
    // 提交期间只给已确认的玩家提供快照。首次分配席位也属于写入，访客不会占住请求线程。
    if (pending_ && !request->body.contains("order")) {
        if (committed_->hasPlayer(request->account)) {
            world_.touch(request->account);
            committed_->touch(request->account);
            request->reply.set_value(committed_->read(request->account, request->body));
        } else if (!committed_->hasOpenSeat()) {
            request->reply.set_value(committed_->read(request->account, request->body));
        } else {
            request->reply.set_value(std::nullopt);
        }
        return;
    }
    if (!ready_ || waiting_.size() >= 32) {
        request->reply.set_value(std::nullopt);
        return;
    }
    waiting_.push_back(request);
    advance();
}

void WorldActor::advance() {
    if (!store_) {
        world_.tick();
        world_.clearChanges();
        return;
    }
    if (fenced_ || !hasLease()) return;
    const bool committed = !pending_ || finishWrite();
    if (fenced_ || !hasLease()) return;
    maybeCheckpoint();
    if (eventCleanup_.completed < coveredEvents_) {
        maybeClean(eventCleanup_, coveredEvents_, "event", [this](auto through) { return store_->pruneEvents(through); });
    }
    if (orderCleanup_.completed < retiredOrdersBefore_) {
        maybeClean(orderCleanup_, retiredOrdersBefore_, "order", [this, active = committed_->seats()](auto before) {
            return store_->pruneOrders(before, active);
        });
    }
    if (!committed) return;
    while (!waiting_.empty()) {
        auto request = std::move(waiting_.front());
        waiting_.pop_front();
        if (Clock::now() >= request->deadline) {
            request->reply.set_value(std::nullopt);
            continue;
        }
        const auto result = world_.update(request->account, request->body);
        if (world_.stateVersion() != committed_->stateVersion()) {
            record(request, result);
            return;
        }
        request->reply.set_value(world_.read(request->account, request->body, result));
    }
    world_.tick();
    const auto now = Clock::now();
    // 普通事件已经记录世界时钟。只有单独发生自然补兵时才定期追加时钟事件。
    if (world_.stateVersion() != committed_->stateVersion() ||
        (now - savedAt_ >= replenishmentSaveInterval && world_.recordReplenishmentSince(savedAt_, now))) record();
}

void WorldActor::record(const std::shared_ptr<Request>& request, World::Result result) {
    const auto at = Clock::now();
    WorldEvent event{world_.event(*committed_, at, writerId_), {}, {}};
    if (auto order = world_.takeOrder()) event.orders.push_back(std::move(*order));
    const auto previous = committed_->seats(), current = world_.seats();
    for (std::size_t index = 0; index < previous.size(); ++index) {
        if (previous[index] != 0 && previous[index] != current[index]) event.retiredSeats.push_back(previous[index]);
    }
    pending_.emplace(PendingWrite{std::make_shared<const WorldEvent>(std::move(event)), request, result, at, {}, {}});
    submitWrite();
}

void WorldActor::submitWrite() {
    auto task = std::make_shared<std::packaged_task<bool()>>(
        [this, revision = savedRevision_, event = pending_->event] { return store_->append(revision, *event, writerId_); });
    auto completion = task->get_future();
    if (writer_.post([this, task] {
            (*task)();
            // 邮箱忙时由常规世界帧收取结果，写线程从不直接更改地图。
            queue_.post([this] { advance(); });
        }) == orbit::QueuePostResult::accepted) {
        pending_->completion = std::move(completion);
    }
}

bool WorldActor::finishWrite() {
    auto& write = *pending_;
    if (!write.completion.valid()) {
        if (Clock::now() >= write.retryAt) submitWrite();
        return false;
    }
    if (write.completion.wait_for(std::chrono::seconds(0)) != std::future_status::ready) return false;
    try {
        if (!write.completion.get()) {
            // 存档被其他进程推进时停止本进程的写入，旧版本不能覆盖新世界。
            fenced_ = true;
            ready_ = false;
            leaseUntil_ = Clock::time_point{};
            std::cerr << "world writer lease or event stream revision was lost\n";
            if (write.request) write.request->reply.set_value(std::nullopt);
            for (const auto& request : waiting_) request->reply.set_value(std::nullopt);
            waiting_.clear();
            pending_.reset();
            return false;
        }
    } catch (const std::exception& error) {
        ready_ = false;
        write.retryAt = Clock::now() + std::chrono::seconds(1);
        std::cerr << error.what() << '\n';
        return false;
    }
    ++savedRevision_;
    savedAt_ = write.recordedAt;
    world_.publish(*committed_);
    if (!write.event->retiredSeats.empty()) retiredOrdersBefore_ = committed_->stateVersion();
    ready_ = true;
    if (write.request) {
        write.request->reply.set_value(hasLease()
            ? Reply{committed_->read(write.request->account, write.request->body, write.result)} : std::nullopt);
    }
    pending_.reset();
    return true;
}

void WorldActor::maybeCheckpoint() {
    const auto now = Clock::now();
    if (checkpointResult_.valid()) {
        if (checkpointResult_.wait_for(std::chrono::seconds(0)) != std::future_status::ready) return;
        try {
            coveredEvents_ = checkpointResult_.get();
            checkpointRevision_ = writingCheckpoint_;
            repairCheckpoint_ = false;
            nextCheckpoint_ = now + checkpointInterval_;
        } catch (const std::exception& error) {
            checkpointRetryAt_ = now + std::chrono::seconds(1);
            std::cerr << "world checkpoint: " << (savedRevision_ - checkpointRevision_) << " events not covered: "
                      << error.what() << '\n';
        }
    }
    if ((repairCheckpoint_ || savedRevision_ > checkpointRevision_) && now >= checkpointRetryAt_ &&
        (now >= nextCheckpoint_ || savedRevision_ - checkpointRevision_ >= checkpointEventLimit)) {
        // 只移交已提交的世界副本，后台序列化不进入指令确认路径。
        auto task = std::make_shared<std::packaged_task<std::uint64_t()>>(
            [this, world = *committed_, at = savedAt_, revision = savedRevision_, repairing = repairCheckpoint_] {
                return store_->checkpoint(revision, world.save(at), repairing);
            });
        auto completion = task->get_future();
        if (checkpointer_.post([task] { (*task)(); }) == orbit::QueuePostResult::accepted) {
            writingCheckpoint_ = savedRevision_;
            checkpointResult_ = std::move(completion);
        }
    }
}

void WorldActor::maybeClean(Cleanup& cleanup, std::uint64_t target, const char* label,
                           std::function<bool(std::uint64_t)> prune) {
    const auto now = Clock::now();
    if (cleanup.result.valid()) {
        if (cleanup.result.wait_for(std::chrono::seconds(0)) != std::future_status::ready) return;
        try {
            if (cleanup.result.get()) cleanup.completed = cleanup.running;
        } catch (const std::exception& error) {
            cleanup.retryAt = now + std::chrono::seconds(1);
            std::cerr << "world " << label << " cleanup at " << cleanup.running << ": " << error.what() << '\n';
        }
    }
    if (cleanup.completed >= target || now < cleanup.retryAt) return;
    auto task = std::make_shared<std::packaged_task<bool()>>([target, prune = std::move(prune)] { return prune(target); });
    auto completion = task->get_future();
    if (cleaner_.post([task] { (*task)(); }) == orbit::QueuePostResult::accepted) {
        cleanup.running = target;
        cleanup.result = std::move(completion);
    }
}

}  // slg 命名空间
