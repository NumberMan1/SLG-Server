cmake_minimum_required(VERSION 3.20)
project(slg_battle_server LANGUAGES CXX)

include(../cpp/orbit.cmake)
slg_orbit_cpp(orbit_source)
add_subdirectory("${orbit_source}" "${CMAKE_CURRENT_BINARY_DIR}/orbit-cpp")

add_library(slg_battle_rules STATIC src/battle.cpp)
target_include_directories(slg_battle_rules PUBLIC include)
set_target_properties(slg_battle_rules PROPERTIES CXX_STANDARD 20 CXX_STANDARD_REQUIRED ON CXX_EXTENSIONS OFF)

add_executable(battle-server src/main.cpp)
target_include_directories(battle-server PRIVATE ../cpp)
target_link_libraries(battle-server PRIVATE slg_battle_rules orbit::cpp)
set_target_properties(battle-server PROPERTIES
    CXX_STANDARD 23
    CXX_STANDARD_REQUIRED ON
    CXX_EXTENSIONS OFF
)

option(SLG_STATIC_BATTLE_SERVER "为 scratch 镜像静态链接战斗服" OFF)
if(SLG_STATIC_BATTLE_SERVER OR MINGW)
    target_link_options(battle-server PRIVATE -static)
endif()

include(CTest)
if(BUILD_TESTING)
    add_test(NAME battle-server.health COMMAND battle-server --healthcheck)
    set_tests_properties(battle-server.health PROPERTIES TIMEOUT 5)
    add_executable(battle-rules-test tests/battle_test.cpp)
    target_link_libraries(battle-rules-test PRIVATE slg_battle_rules)
    set_target_properties(battle-rules-test PROPERTIES CXX_STANDARD 20 CXX_STANDARD_REQUIRED ON CXX_EXTENSIONS OFF)
    if(MINGW)
        target_link_options(battle-rules-test PRIVATE -static)
    endif()
    add_test(NAME battle-server.rules COMMAND battle-rules-test)
endif()
