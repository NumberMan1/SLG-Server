add_library(slg_battle_rules STATIC src/battle.cpp)
target_include_directories(slg_battle_rules PUBLIC include)
set_target_properties(slg_battle_rules PROPERTIES
    CXX_STANDARD 20
    CXX_STANDARD_REQUIRED ON
    CXX_EXTENSIONS OFF
)

if(BUILD_TESTING)
    add_executable(battle-rules-test tests/battle_test.cpp)
    target_link_libraries(battle-rules-test PRIVATE slg_battle_rules)
    set_target_properties(battle-rules-test PROPERTIES CXX_STANDARD 20 CXX_STANDARD_REQUIRED ON CXX_EXTENSIONS OFF)
    if(MINGW)
        target_link_options(battle-rules-test PRIVATE -static)
    endif()
    add_test(NAME battle.rules COMMAND battle-rules-test)
endif()
