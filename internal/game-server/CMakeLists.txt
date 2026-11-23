cmake_minimum_required(VERSION 3.20)
project(slg_game_server LANGUAGES CXX)

include(dependencies.cmake)
include(../cpp/orbit.cmake)
slg_orbit_cpp(orbit_source)
add_subdirectory("${orbit_source}" "${CMAKE_CURRENT_BINARY_DIR}/orbit-cpp")
find_package(Threads REQUIRED)

include(CTest)
add_subdirectory(../battle "${CMAKE_CURRENT_BINARY_DIR}/battle")
add_executable(game-server src/main.cpp src/world.cpp src/world_actor.cpp)
set_target_properties(game-server PROPERTIES
    CXX_STANDARD 23
    CXX_STANDARD_REQUIRED ON
    CXX_EXTENSIONS OFF
)
target_include_directories(game-server PRIVATE "${SLG_GAME_DEPS_DIR}" ../cpp)
target_link_libraries(game-server PRIVATE orbit::cpp slg_battle_rules Threads::Threads)
if(WIN32)
    target_compile_definitions(game-server PRIVATE _WIN32_WINNT=0x0A00)
    target_link_libraries(game-server PRIVATE ws2_32)
endif()

option(SLG_STATIC_GAME_SERVER "为 scratch 镜像静态链接游戏服" OFF)
if(SLG_STATIC_GAME_SERVER OR MINGW)
    # Windows 测试进程也使用静态工具链运行库，避免 Git Bash 加载不同版本的 DLL。
    target_link_options(game-server PRIVATE -static)
endif()
