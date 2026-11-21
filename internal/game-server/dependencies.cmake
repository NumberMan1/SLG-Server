if(NOT DEFINED SLG_GAME_DEPS_DIR)
    set(SLG_GAME_DEPS_DIR "${CMAKE_CURRENT_BINARY_DIR}/deps")
endif()

# 本机和容器构建使用相同的第三方头文件，缓存文件也必须通过摘要校验。
function(slg_game_header name url sha)
    file(MAKE_DIRECTORY "${SLG_GAME_DEPS_DIR}")
    set(path "${SLG_GAME_DEPS_DIR}/${name}")
    file(LOCK "${path}.lock" GUARD FUNCTION TIMEOUT 150)
    if(EXISTS "${path}")
        file(SHA256 "${path}" actual)
        if(actual STREQUAL sha)
            return()
        endif()
    endif()
    file(DOWNLOAD "${url}" "${path}.tmp" EXPECTED_HASH "SHA256=${sha}"
        TLS_VERIFY ON TIMEOUT 120 INACTIVITY_TIMEOUT 20)
    file(RENAME "${path}.tmp" "${path}")
endfunction()

slg_game_header(json.hpp
    "https://raw.githubusercontent.com/nlohmann/json/v3.12.0/single_include/nlohmann/json.hpp"
    aaf127c04cb31c406e5b04a63f1ae89369fccde6d8fa7cdda1ed4f32dfc5de63)
slg_game_header(httplib.h
    "https://raw.githubusercontent.com/yhirose/cpp-httplib/v0.28.0/httplib.h"
    9e5cafaa227dad5c10e837990a736cecaff03258f50d37067cedeee1dba5d344)
