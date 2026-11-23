#include "world_actor.hpp"

#include <future>
#include <iostream>
#include <memory>
#include <stdexcept>
#include <utility>

namespace slg {

WorldActor::WorldActor(std::chrono::seconds seatIdleTimeout)
    : world_(seatIdleTimeout), queue_(64, {
          .interval = std::chrono::milliseconds(50),
          .update = [this](auto) { world_.tick(); },
          .on_error = [](std::exception_ptr error) {
              try {
                  std::rethrow_exception(error);
              } catch (const std::exception& exception) {
                  std::cerr << "world update: " << exception.what() << '\n';
              }
          },
      }) {
    if (queue_.start() != orbit::QueueStartResult::started) {
        throw std::runtime_error("game-server failed to start world mailbox");
    }
}

std::optional<nlohmann::json> WorldActor::update(std::string account, nlohmann::json request) {
    auto task = std::make_shared<std::packaged_task<nlohmann::json()>>(
        [this, account = std::move(account), request = std::move(request)] { return world_.update(account, request); });
    auto response = task->get_future();
    if (queue_.post([task] { (*task)(); }) != orbit::QueuePostResult::accepted) {
        return std::nullopt;
    }
    return response.get();
}

}  // slg 命名空间
