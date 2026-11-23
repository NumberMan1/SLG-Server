#pragma once

#include <orbit/serial_queue.hpp>

#include "world.hpp"

namespace slg {

// 用 Orbit 队列独占一张地图。网络线程通过这里提交命令，队列内执行地图更新和交锋。
class WorldActor {
public:
    explicit WorldActor(std::chrono::seconds seatIdleTimeout);

    // 已受理的命令执行到完成。邮箱关闭或已满时返回空值，由网关允许客户端重试。
    std::optional<nlohmann::json> update(std::string account, nlohmann::json request);
private:
    World world_;
    // 销毁队列会先排空并回收线程，之后才销毁世界状态。
    orbit::SerialQueue queue_;
};

}  // slg 命名空间
