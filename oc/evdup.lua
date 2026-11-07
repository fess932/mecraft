-- evdup: looks for a rule that tells AE2's duplicate change events apart.
-- usage: evdup "<full label>" [seconds]
-- Records the events about one item with their time, prints them, then
-- sums the sizes under several dedup rules next to the real change of
-- the stock, so the rule that matches the stock shows.
local component = require("component")
local computer = require("computer")
local event = require("event")

local want, secs = ...
secs = tonumber(secs) or 20
if not want then
  print('usage: evdup "<полное название>" [секунд]')
  return
end

local me
for _, t in ipairs({ "me_controller", "me_interface" }) do
  if component.isAvailable(t) then
    me = component.getPrimary(t)
    break
  end
end
if not me then
  print("нет ME-компонента")
  return
end

local function stock()
  local total = 0
  for _, s in ipairs(me.getItemsInNetwork({ label = want }) or {}) do
    total = total + (s.size or 0)
  end
  return total
end

-- one record per event that mentions the item:
-- t = uptime (1 tick = 0.05 s), n = stacks in the event, sizes = the
-- item's sizes in it, sig = all stacks of the event as text
local recs = {}
local function onEvent(sig)
  local sizes, all = {}, {}
  for i = 3, sig.n do
    local s = sig[i]
    if type(s) == "table" then
      all[#all + 1] = tostring(s.label) .. "=" .. tostring(s.size)
      if s.label == want then
        sizes[#sizes + 1] = s.size or 0
      end
    end
  end
  if #sizes > 0 then
    recs[#recs + 1] = { t = computer.uptime(), n = sig.n - 2, sizes = sizes, sig = table.concat(all, ";") }
  end
end

pcall(me.setItemEventSubscription, false)
pcall(me.setItemEventSubscription, true)
local before = stock()
print("было " .. before .. ", пишу " .. secs .. " с...")
local deadline = computer.uptime() + secs
while computer.uptime() < deadline do
  local sig = table.pack(event.pull(0.05))
  if sig[1] == "network_item_changed" then
    onEvent(sig)
  end
end
local after = stock()
pcall(me.setItemEventSubscription, false)

local t0 = recs[1] and recs[1].t or 0
local function ticks(r) return math.floor((r.t - t0) / 0.05 + 0.5) end
local function sum(r) local s = 0 for _, v in ipairs(r.sizes) do s = s + v end return s end

print("события (тик, записей в событии, размеры):")
for i, r in ipairs(recs) do
  if i > 40 then
    print("  ... ещё " .. (#recs - 40))
    break
  end
  local same = recs[i - 1] and recs[i - 1].sig == r.sig and " = копия предыдущего" or ""
  print(string.format("  %4d  %3d  %s%s", ticks(r), r.n, table.concat(r.sizes, ","), same))
end

-- the rules: drop an event whose stacks are exactly those of the previous
-- event about the item, if it came within maxGap ticks (nil = any gap)
local function total(maxGap, bigOut)
  local s, prev = 0, nil
  for _, r in ipairs(recs) do
    local dup = prev and prev.sig == r.sig and (maxGap == nil or ticks(r) - ticks(prev) <= maxGap)
    if not dup then
      for _, v in ipairs(r.sizes) do
        if not (bigOut and math.abs(v) >= bigOut) then
          s = s + v
        end
      end
    end
    prev = r
  end
  return s
end

local real = after - before
print("стало " .. after .. ", реальная разница " .. real)
print("сумма как есть:                 " .. total(-1))
print("без копий в тот же тик:         " .. total(0))
print("без копий в пределах 1 тика:    " .. total(1))
print("без копий в пределах 5 тиков:   " .. total(5))
print("без любых копий подряд:         " .. total(nil))
local big = math.max(1000, math.floor(before / 2))
print("без копий и без записей >= " .. big .. ": " .. total(nil, big))

-- what mecraft does: drop every second copy within the same tick, net the
-- event, and skip a change of 90% of the stock or more (stock >= 1000)
local s, have, prev, dropped = 0, before, nil, false
for _, r in ipairs(recs) do
  local copy = prev and prev.sig == r.sig and ticks(r) == ticks(prev) and r.n <= 8 and not dropped
  if copy then
    dropped = true
  else
    dropped = false
    local n = sum(r)
    if not (have >= 1000 and math.abs(n) >= 0.9 * have) then
      s, have = s + n, have + n
    end
  end
  prev = r
end
print("правило mecraft:                " .. s)
