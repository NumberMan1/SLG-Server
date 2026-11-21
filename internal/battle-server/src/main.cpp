#include <iostream>
#include <string_view>

int main(int argc, char* argv[]) {
    // CTest 通过退出码验证战斗服可执行文件能启动并处理健康检查参数。
    if (argc == 2 && std::string_view(argv[1]) == "--healthcheck") {
        std::cout << "battle-server: ok\n";
        return 0;
    }

    std::cerr << "Usage: battle-server --healthcheck\n";
    return 2;
}
