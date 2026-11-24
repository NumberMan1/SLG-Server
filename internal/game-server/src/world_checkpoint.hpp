#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace slg {

// 指令记录按玩家席位增量写入。席位版本在回收后永不重用。
struct SavedOrder {
    std::uint64_t seat;
    std::string id;
    int target;
    std::uint32_t troops;
    int mode;
    std::uint64_t reinforcement, retreat;
};

// 同一版本的事件、成功指令和席位回收由一个存储事务提交。
struct WorldEvent {
    std::string payload;
    std::vector<SavedOrder> orders;
    std::vector<std::uint64_t> retiredSeats;
};

}  // slg 命名空间
