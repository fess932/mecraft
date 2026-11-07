-- evprobe: shows what network_item_changed really carries for one item.
-- usage: evprobe <part of the label> [seconds]
-- Prints the first events as they arrive, then how many there were and
-- what their sizes add up to, next to the stock before and after.
local component = require("component")
local computer = require("computer")
local event = require("event")
local ser = require("serialization")

local want, secs = ...
secs = tonumber(secs) or 10
if not want then
  print("usage: evprobe <часть названия> [секунд]")
  return
end

local me
for _, t in ipairs({ "me_controller", "me_interface" }) do
  if component.isAvailable(t) then
    me = component.getPrimary(t)
    break
  end
end

local function stock()
  if not me then return "?" end
  local total, found = 0, false
  for _, s in ipairs(me.getItemsInNetwork({ label = want }) or {}) do
    total, found = total + (s.size or 0), true
  end
  if found then return total end
  -- the filter wants the whole label; fall back to a search
  for _, s in ipairs(me.getItemsInNetwork() or {}) do
    if s.label and s.label:find(want, 1, true) then
      total = total + (s.size or 0)
    end
  end
  return total
end

local shown, events, matched, sum, plus, minus = 0, 0, 0, 0, 0, 0
local function on(name, ...)
  events = events + 1
  local args = table.pack(...)
  for i = 1, args.n do
    local s = args[i]
    if type(s) == "table" and s.label and s.label:find(want, 1, true) then
      matched = matched + 1
      local n = s.size or 0
      sum = sum + n
      if n > 0 then plus = plus + n else minus = minus - n end
      if shown < 8 then
        shown = shown + 1
        print(string.format("событие #%d, аргумент %d из %d: %s", events, i, args.n, ser.serialize(s)))
      end
    end
  end
end

-- AE2 sends the events only once someone subscribed; mecraft does it at
-- start. Turn it on here too and leave it on: mecraft may be running.
if me and me.setItemEventSubscription then
  print("подписка: " .. tostring(select(2, pcall(me.setItemEventSubscription, true))))
else
  print("подписки на события нет в этом компоненте")
end

-- Pull every signal the way mecraft does and count them by name, so a
-- different event name or none at all shows up too.
local before = stock()
print("в сети было: " .. tostring(before) .. ", слушаю " .. secs .. " с...")
local names, firstOther = {}, nil
local deadline = computer.uptime() + secs
while computer.uptime() < deadline do
  local sig = table.pack(event.pull(0.25))
  if sig[1] then
    names[sig[1]] = (names[sig[1]] or 0) + 1
    if sig[1] == "network_item_changed" then
      on(table.unpack(sig, 1, sig.n))
    elseif not firstOther and tostring(sig[1]):find("network", 1, true) then
      firstOther = ser.serialize(sig)
    end
  end
end
local after = stock()
for n, c in pairs(names) do
  print("сигнал " .. tostring(n) .. ": " .. c)
end
if firstOther then
  print("пример: " .. firstOther:sub(1, 300))
end
print("событий network_item_changed: " .. events .. ", записей про предмет: " .. matched)
print("сумма size: " .. sum .. " (плюсы " .. plus .. ", минусы " .. minus .. ")")
print("в сети стало: " .. tostring(after) .. ", разница: " ..
  ((type(before) == "number" and type(after) == "number") and tostring(after - before) or "?"))
