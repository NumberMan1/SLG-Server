# C++ 和 Go 共用 go.mod 锁定的 Orbit 源码。容器构建使用宿主机暂存的同一目录。
function(slg_orbit_cpp output)
    if(SLG_ORBIT_CPP_DIR)
        set(source "${SLG_ORBIT_CPP_DIR}")
    elseif(NOT "$ENV{SLG_ORBIT_CPP_DIR}" STREQUAL "")
        set(source "$ENV{SLG_ORBIT_CPP_DIR}")
    else()
        find_program(go_executable go REQUIRED)
        execute_process(
            COMMAND "${go_executable}" mod download -json github.com/NumberMan1/orbit
            WORKING_DIRECTORY "${CMAKE_CURRENT_FUNCTION_LIST_DIR}/../.."
            OUTPUT_VARIABLE metadata
            RESULT_VARIABLE result
        )
        if(NOT result EQUAL 0)
            message(FATAL_ERROR "无法获取 go.mod 锁定的 Orbit 源码，请检查 Go 私有模块访问配置")
        endif()
        string(JSON module_directory GET "${metadata}" Dir)
        set(source "${module_directory}/cpp")
    endif()
    if(NOT EXISTS "${source}/include/orbit/request_server.hpp" OR
       NOT EXISTS "${source}/include/orbit/tcp_endpoint.hpp")
        message(FATAL_ERROR "Orbit C++ 源码不完整，请使用 v0.4.1 或更新版本：${source}")
    endif()
    set(${output} "${source}" PARENT_SCOPE)
endfunction()

if(SLG_ORBIT_STAGE_DIR)
    slg_orbit_cpp(source)
    file(COPY "${source}/CMakeLists.txt" "${source}/include" "${source}/src"
        DESTINATION "${SLG_ORBIT_STAGE_DIR}")
endif()
