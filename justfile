set windows-shell := ["pwsh.exe", "-NoLogo", "-NoProfile", "-Command"]

instance := env("GTNH_INSTANCE", home_directory() / "AppData/Roaming/PrismLauncher/instances/GT_New_Horizons_2.9.0_Java_17-26/.minecraft")
icons_dump := instance / "dumps/itempanel_icons"
bin := if os() == "windows" { "mecraft.exe" } else { "mecraft" }

# список команд
default:
    @just --list

# скопировать иконки из дампа NEI в assets/icons
icons dump=icons_dump:
    go run ./cmd/importicons -src "{{dump}}"

# собрать бинарник
build:
    go build -o {{bin}} .

# собрать и запустить сервер: just run [токен]; остальное в mecraft.json
run token="": build
    ./{{bin}} {{ if token != "" { "-token " + quote(token) } else { "" } }}

# иконки + сборка с нуля
all: icons build

# go vet и проверка форматирования
check:
    go vet ./...
    gofmt -l .

# собрать под Linux
build-linux:
    {{ if os() == "windows" { "$env:GOOS='linux'; $env:GOARCH='amd64'; go build -o mecraft-linux .; Remove-Item Env:GOOS, Env:GOARCH" } else { "GOOS=linux GOARCH=amd64 go build -o mecraft-linux ." } }}

# удалить бинарники
clean:
    {{ if os() == "windows" { "Remove-Item -ErrorAction SilentlyContinue " + bin + ", mecraft-linux" } else { "rm -f " + bin + " mecraft-linux" } }}
