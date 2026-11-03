# mecraft

Веб-страница с содержимым ME-сети из GTNH: предметы с иконками, жидкости,
процессоры крафта и заказы крафта прямо из браузера.

Компьютер OpenComputers в игре держит TCP-соединение с сервером на Go и
шлёт ему состояние сети; сервер показывает его на странице (htmx) и
передаёт заказы обратно в игру. Шаблоны, стили, htmx, иконки и Lua-скрипты
вшиты в бинарник.

```
OC (oc/mecraft.lua) ──TCP, JSON по строкам──► mecraft (Go) ◄──HTTP── браузер
```

## Сервер

Нужен Go 1.24+.

1. В игре выгрузи иконки: NEI → Options → Tools → Data Dumps →
   «Панель предметов», переключи формат на **PNG** и нажми «Выгрузить».
   Поиск NEI перед этим очисти, иначе выгрузится только найденное.
2. Скопируй иконки в репозиторий (в git они не попадают):
   ```
   go run ./cmd/importicons -src "<.minecraft>/dumps/itempanel_icons"
   ```
3. Собери и запусти:
   ```
   go build -o mecraft.exe .
   ./mecraft.exe -http :8080 -oc :7777 -token секрет
   ```
   Страница: http://localhost:8080

Флаги: `-http` адрес страницы, `-oc` порт для OC, `-token` пароль, который
должен прислать OC (пустой — без проверки).

Без шага 2 всё работает, только вместо иконок будут заглушки.

## Компьютер в игре

Нужно:
- **Internet Card**;
- **Adapter** вплотную к ME Controller или ME Interface;
- побольше памяти: полный список сети приходит одной таблицей.

Установка (в шелле OpenOS):

```
wget -f https://raw.githubusercontent.com/fess932/mecraft/main/oc/json.lua /lib/json.lua
wget -f https://raw.githubusercontent.com/fess932/mecraft/main/oc/mecraft.lua /bin/mecraft.lua
```

Запуск:

```
mecraft <IP сервера> 7777 секрет
```

Остановка: Ctrl+C. При обрыве скрипт сам переподключается каждые 5 секунд.
Чтобы запускался при загрузке, добавь строку запуска в `/home/.shrc`.

### Если сервер на этом же компе или в локальной сети

OpenComputers по умолчанию запрещает подключения к `127.0.0.1` и локальным
адресам (`192.168.*`, `10.*`, `172.16–31.*`). В
`config/OpenComputers.cfg`, секция `internet`, добавь разрешение
**первой** строкой `filteringRules`, указав IP сервера:

```
filteringRules=[
  "allow ip:192.168.1.10",
  "deny ip:127.0.0.0/8",
  ...
]
```

Перезапусти игру.

## Как это работает

- При подключении OC шлёт полный список предметов и жидкостей, дальше
  только изменения (события `network_item_changed` /
  `network_fluid_changed`) раз в полсекунды. Раз в 5 минут и по кнопке
  «Синхронизировать» список шлётся целиком заново, чтобы поправить
  пропущенные события.
- Процессоры и энергия ME обновляются раз в 5 секунд.
- Иконка ищется по названию предмета, как его видит игра (`label`), тем же
  правилом, по которому NEI называет файлы. Если в игре и при выгрузке
  иконок разный язык, иконки не найдутся: выгружай на том же языке.
- Предметы, у которых совпадают ID и подтип, различаются по названию: NBT
  в OC не передаётся.

## Протокол

Одно JSON-сообщение на строку, поле `type` — тип.

OC → сервер: `hello {token}`, `items_begin`, `items {items}`, `items_end`,
`item_delta {items}` (size со знаком), `fluids {fluids}`,
`fluid_delta {fluids}`, `cpus {cpus}`, `power {stored,max,usage,injection}`,
`job {job,state,reason}`.

Сервер → OC: `craft {job,name,damage,label,amount}`, `resync`.

## Лицензии

`oc/json.lua` — [rxi/json.lua](https://github.com/rxi/json.lua), MIT.
`assets/static/htmx.min.js` — [htmx](https://htmx.org) 2.0.4, BSD Zero Clause.
