-- evsrc: finds out where duplicate ME change events come from.
-- usage: evsrc [seconds per step]
-- Lists the components, then turns event subscriptions off everywhere
-- and on for one ME component at a time, counting the events per source
-- and how many repeat the previous one exactly.
local component = require("component")
local computer = require("computer")
local event = require("event")

local secs = tonumber((...)) or 5

print("компоненты:")
local mes = {}
for addr, t in component.list() do
  local p = component.proxy(addr)
  local sub = p and p.setItemEventSubscription and " (умеет подписку)" or ""
  print("  " .. addr:sub(1, 8) .. "  " .. t .. sub)
  if sub ~= "" then
    mes[#mes + 1] = p
  end
end

-- summary of one event: labels and sizes of its stacks
local function key(sig)
  local parts = {}
  for i = 3, sig.n do
    local s = sig[i]
    if type(s) == "table" then
      parts[#parts + 1] = tostring(s.label) .. "=" .. tostring(s.size)
    end
  end
  return table.concat(parts, ";")
end

local function listen(title)
  local bySrc, total, repeats, last = {}, 0, 0, nil
  local deadline = computer.uptime() + secs
  while computer.uptime() < deadline do
    local sig = table.pack(event.pull(0.25))
    if sig[1] == "network_item_changed" then
      total = total + 1
      local src = tostring(sig[2]):sub(1, 8)
      bySrc[src] = (bySrc[src] or 0) + 1
      local k = src .. "|" .. key(sig)
      if k == last then
        repeats = repeats + 1
      end
      last = k
    end
  end
  print(title .. ": событий " .. total .. ", копий подряд " .. repeats)
  for src, n in pairs(bySrc) do
    print("    от " .. src .. ": " .. n)
  end
end

for _, p in ipairs(mes) do
  pcall(p.setItemEventSubscription, false)
end
listen("все подписки выключены")
for _, p in ipairs(mes) do
  pcall(p.setItemEventSubscription, true)
  listen("подписка только на " .. p.address:sub(1, 8))
  pcall(p.setItemEventSubscription, false)
end
if #mes > 1 then
  for _, p in ipairs(mes) do
    pcall(p.setItemEventSubscription, true)
  end
  listen("подписка на все сразу")
  for _, p in ipairs(mes) do
    pcall(p.setItemEventSubscription, false)
  end
end
print("готово; подписки выключены, mecraft включит свою сам")
