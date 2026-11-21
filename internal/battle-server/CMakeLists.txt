cmake_minimum_required(VERSION 3.20)
project(slg_battle_server LANGUAGES CXX)

add_executable(battle-server src/main.cpp)
set_target_properties(battle-server PROPERTIES
    CXX_STANDARD 20
    CXX_STANDARD_REQUIRED ON
    CXX_EXTENSIONS OFF
)

include(CTest)
if(BUILD_TESTING)
    add_test(NAME battle-server.health COMMAND battle-server --healthcheck)
    set_tests_properties(battle-server.health PROPERTIES TIMEOUT 5)
endif()
