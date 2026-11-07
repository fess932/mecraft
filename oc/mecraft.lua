-- mecraft: streams the ME network to the mecraft server and runs its
-- crafting orders.
--
-- Needs: Internet Card and an Adapter touching an ME Controller or ME
-- Interface. Messages are Lua tables, one per line, written and read with
-- the built-in serialization library.
-- Usage: mecraft <host> [port] [token]

local component = require("component")
local computer = require("computer")
local event = require("event")
local serialization = require("serialization")

local args = { ... }
local HOST = args[1]
local PORT = tonumber(args[2] or "7777")
local TOKEN = args[3] or ""

if not HOST then
  io.stderr:write("usage: mecraft <host> [port] [token]\n")
  return
end

local BATCH = 64           -- stacks per message (small: OC memory is tight)
local FLUSH_EVERY = 0.5    -- seconds between sending accumulated changes
local STATUS_EVERY = 5     -- seconds between CPU/power reports
local CPU_PARTS = 32       -- items in progress reported per busy CPU
local RESYNC_EVERY = 300   -- seconds between full resyncs (fixes lost events)

if not component.isAvailable("internet") then
  error("нужна Internet Card", 0)
end
local inet = component.internet

local me, meType
for _, t in ipairs({ "me_controller", "me_interface" }) do
  if component.isAvailable(t) then
    me, meType = component.getPrimary(t), t
    break
  end
end
if not me then
  error("не вижу ME: поставь Adapter вплотную к ME Controller или ME Interface", 0)
end

-- Values coming through signals pass via NBT, so booleans may arrive as 1/0.
local function truthy(v)
  return v == true or v == 1
end

-- Errors that retrying will not fix stop the script instead of reconnecting.
local FATAL = {}

local function fatal(msg)
  error(setmetatable({ msg = msg }, FATAL), 0)
end

local function isFatal(e)
  if getmetatable(e) == FATAL then
    return true
  end
  local s = tostring(e)
  return s:find("not enough memory", 1, true) or s:find("out of memory", 1, true)
end

local function fatalText(e)
  local msg = getmetatable(e) == FATAL and e.msg or tostring(e)
  local text = msg .. string.format(". Память OC: свободно %d КБ из %d КБ",
    math.floor(computer.freeMemory() / 1024), math.floor(computer.totalMemory() / 1024))
  if msg:find("memory", 1, true) then
    text = text .. ". Не хватает памяти: поставь планки побольше (до тира 3.5) или вторую планку"
  end
  return text
end

local function itemOf(s)
  return {
    name = s.name,
    damage = s.damage or 0,
    label = s.label or s.name,
    size = s.size or 0,
    hasTag = truthy(s.hasTag),
    isCraftable = truthy(s.isCraftable),
  }
end

local function fluidOf(s)
  return {
    name = s.name,
    label = s.label or s.name,
    amount = s.amount or 0,
    isCraftable = truthy(s.isCraftable),
  }
end

local function count(t)
  return t.n or #t
end

------------------------------------------------------------------------
-- socket

local sock
local inbuf = ""

local function connect()
  local s, err = inet.connect(HOST, PORT)
  if not s then
    return nil, err
  end
  local deadline = computer.uptime() + 10
  while true do
    local ok, res = pcall(s.finishConnect)
    if not ok then
      pcall(s.close)
      return nil, res
    end
    if res then
      return s
    end
    if computer.uptime() > deadline then
      pcall(s.close)
      return nil, "timeout"
    end
    os.sleep(0.1)
  end
end

-- sendLine writes one already encoded message.
local function sendLine(data)
  data = data .. "\n"
  local pos = 1
  while pos <= #data do
    local n, err = sock.write(data:sub(pos))
    if not n then
      error("write: " .. tostring(err), 0)
    end
    pos = pos + n
    if pos <= #data then
      os.sleep(0.05)
    end
  end
end

-- Small messages go through the stock serializer.
local function send(msg)
  sendLine(serialization.serialize(msg))
end

-- Big lists are encoded by hand, one string per stack, in the same format
-- serialization.serialize writes. That avoids building thousands of tiny
-- strings per message, which runs small computers out of memory.
local fmt, tostr = string.format, tostring

local function quote(s)
  return (fmt("%q", tostr(s)):gsub("\\\n", "\\n"))
end

local function bool(v)
  return truthy(v) and "true" or "false"
end

local function num(v)
  v = tonumber(v) or 0
  if v ~= v or v == math.huge or v == -math.huge then
    return "0"
  end
  return tostr(v)
end

local function encItem(s)
  return "{name=" .. quote(s.name) .. ",damage=" .. num(s.damage) ..
    ",label=" .. quote(s.label or s.name) .. ",size=" .. num(s.size) ..
    ",hasTag=" .. bool(s.hasTag) .. ",isCraftable=" .. bool(s.isCraftable) .. "}"
end

local function encFluid(s)
  return "{name=" .. quote(s.name) .. ",label=" .. quote(s.label or s.name) ..
    ",amount=" .. num(s.amount) .. ",isCraftable=" .. bool(s.isCraftable) .. "}"
end

-- sendList sends parts (encoded stacks) as {type=typ,field={...}}.
local function sendList(typ, field, parts)
  sendLine('{type="' .. typ .. '",' .. field .. "={" .. table.concat(parts, ",") .. "}}")
end

------------------------------------------------------------------------
-- ME change events: collected by listeners, sent in batches

local itemDelta, fluidDelta = {}, {}

local function onItems(_, ...)
  for i = 1, select("#", ...) do
    local s = select(i, ...)
    if type(s) == "table" and s.name then
      local k = s.name .. "|" .. tostring(s.damage or 0) .. "|" .. tostring(s.label)
      local d = itemDelta[k]
      if d then
        d.size = d.size + (s.size or 0)
      else
        itemDelta[k] = itemOf(s)
      end
    end
  end
end

local function onFluids(_, ...)
  for i = 1, select("#", ...) do
    local s = select(i, ...)
    if type(s) == "table" and s.name then
      local d = fluidDelta[s.name]
      if d then
        d.amount = d.amount + (s.amount or 0)
      else
        fluidDelta[s.name] = fluidOf(s)
      end
    end
  end
end

local function flushDeltas()
  local items, fluids = itemDelta, fluidDelta
  itemDelta, fluidDelta = {}, {}

  local parts = {}
  for _, v in pairs(items) do
    if v.size ~= 0 then
      parts[#parts + 1] = encItem(v)
      if #parts >= BATCH then
        sendList("item_delta", "items", parts)
        parts = {}
      end
    end
  end
  if #parts > 0 then
    sendList("item_delta", "items", parts)
  end

  parts = {}
  for _, v in pairs(fluids) do
    if v.amount ~= 0 then
      parts[#parts + 1] = encFluid(v)
    end
  end
  if #parts > 0 then
    sendList("fluid_delta", "fluids", parts)
  end
end

------------------------------------------------------------------------
-- full state

local function fullSync()
  itemDelta, fluidDelta = {}, {}
  sendLine('{type="items_begin"}')
  local parts = {}
  local function add(s)
    parts[#parts + 1] = encItem(s)
    if #parts >= BATCH then
      sendList("items", "items", parts)
      parts = {}
    end
  end

  -- The whole list arrives as one table, so it needs memory for every
  -- stack at once. If it does not fit, stop with a clear message.
  local ok, items = pcall(me.getItemsInNetwork)
  if not ok or type(items) ~= "table" then
    fatal("не получил список ME: " .. tostr(items))
  end
  for i = 1, count(items) do
    add(items[i])
    items[i] = nil -- let the collector take stacks already sent
  end
  if #parts > 0 then
    sendList("items", "items", parts)
  end
  items, parts = nil, nil
  sendLine('{type="items_end"}')

  local fluids = me.getFluidsInNetwork()
  parts = {}
  for i = 1, count(fluids) do
    parts[#parts + 1] = encFluid(fluids[i])
    fluids[i] = nil
  end
  sendList("fluids", "fluids", parts)
end

local function sendStatus()
  local cpus = me.getCpus()
  local out = {}
  for i = 1, count(cpus) do
    local c = cpus[i]
    -- What a busy CPU crafts, so the server never orders the same item
    -- again while it is still in progress, even after a reconnect.
    local output, parts
    -- c.cpu is an OC value (userdata), so call it inside pcall.
    -- finalOutput needs a Crafting Monitor on the CPU; without one, report
    -- what the CPU crafts now and what it still has to craft.
    if truthy(c.busy) and c.cpu ~= nil then
      local ok, st = pcall(function() return c.cpu.finalOutput() end)
      if ok and type(st) == "table" and st.name then
        output = itemOf(st)
      else
        parts = {}
        for _, m in ipairs({ "activeItems", "pendingItems" }) do
          local lok, list = pcall(function() return c.cpu[m]() end)
          if lok and type(list) == "table" then
            for j = 1, count(list) do
              local s = list[j]
              if #parts >= CPU_PARTS then break end
              if type(s) == "table" and s.name then
                parts[#parts + 1] = { name = s.name, damage = s.damage or 0, label = s.label or s.name }
              end
            end
          end
        end
      end
    end
    out[#out + 1] = {
      name = c.name or "",
      storage = c.storage or 0,
      coprocessors = c.coprocessors or 0,
      busy = truthy(c.busy),
      output = output,
      parts = parts,
    }
  end
  send({ type = "cpus", cpus = out })
  send({
    type = "power",
    stored = me.getStoredPower(),
    max = me.getMaxStoredPower(),
    usage = me.getAvgPowerUsage(),
    injection = me.getAvgPowerInjection(),
    memFree = computer.freeMemory(),
    memTotal = computer.totalMemory(),
  })
end

------------------------------------------------------------------------
-- crafting orders

local jobs = {}

-- AE2 keeps fluids as their own stack type; OC asks for one by name.
local function fluidCraftable(name)
  if not me.getCraftable then
    return nil, "этот OC не умеет заказывать жидкости (нет getCraftable)"
  end
  return me.getCraftable({ name = name }, "fluid"), "нет шаблона на жидкость"
end

local function itemCraftable(msg)
  local list = me.getCraftables({ name = msg.name, damage = msg.damage })
  local n = count(list)
  local pick
  if n == 1 then
    pick = list[1]
  else
    for i = 1, n do
      local st = list[i].getStack()
      if st and st.label == msg.label then
        pick = list[i]
        break
      end
    end
    pick = pick or list[1]
  end
  return pick, "нет рецепта"
end

local function craft(msg)
  local pick, why
  if msg.fluid then
    pick, why = fluidCraftable(msg.name)
  else
    pick, why = itemCraftable(msg)
  end
  if not pick then
    send({ type = "job", job = msg.job, state = "failed", reason = why })
    return
  end
  local status, err = pick.request(msg.amount or 1)
  if not status then
    send({ type = "job", job = msg.job, state = "failed", reason = tostring(err) })
    return
  end
  jobs[msg.job] = { status = status, state = "computing" }
  send({ type = "job", job = msg.job, state = "computing" })
end

local function pollJobs()
  for id, j in pairs(jobs) do
    local st = j.status
    local state, reason
    local failed, why = st.hasFailed()
    -- OC bug: saving the world marks every unfinished request as failed
    -- with the default reason "no link", while AE2 keeps crafting. A real
    -- failure says "request failed (...)", so trust isDone/isCanceled here.
    if failed and why == "no link" then
      failed = false
    end
    if failed then
      state, reason = "failed", why
    elseif st.isComputing() then
      state = "computing"
    elseif st.isCanceled() then
      state = "canceled"
    elseif st.isDone() then
      state = "done"
    else
      state = "running"
    end
    if state ~= j.state then
      j.state = state
      send({ type = "job", job = id, state = state, reason = reason })
    end
    if state == "failed" or state == "canceled" or state == "done" then
      jobs[id] = nil
    end
  end
end

------------------------------------------------------------------------
-- incoming commands

local needResync = false

local function handle(line)
  local msg = serialization.unserialize(line)
  if type(msg) ~= "table" then
    return
  end
  if msg.type == "craft" then
    local cok, err = pcall(craft, msg)
    if not cok then
      send({ type = "job", job = msg.job, state = "failed", reason = tostring(err) })
    end
  elseif msg.type == "resync" then
    needResync = true
  elseif msg.type == "ping" then
    send({ type = "pong" })
  elseif msg.type == "error" then
    print("mecraft: сервер: " .. tostring(msg.message))
  end
end

local function readSocket()
  while true do
    local chunk, err = sock.read(8192)
    if chunk == nil then
      error("соединение закрыто: " .. tostring(err), 0)
    end
    if chunk == "" then
      break
    end
    inbuf = inbuf .. chunk
  end
  while true do
    local nl = inbuf:find("\n", 1, true)
    if not nl then
      break
    end
    local line = inbuf:sub(1, nl - 1)
    inbuf = inbuf:sub(nl + 1)
    if #line > 0 then
      handle(line)
    end
  end
end

------------------------------------------------------------------------
-- main loop

local function session()
  inbuf = ""
  send({ type = "hello", token = TOKEN, version = 1, me = meType })
  me.setItemEventSubscription(true)
  me.setFluidEventSubscription(true)
  fullSync()
  sendStatus()
  print("mecraft: подключено, синхронизация отправлена")

  local now = computer.uptime()
  local nextFlush = now + FLUSH_EVERY
  local nextStatus = now + STATUS_EVERY
  local nextResync = now + RESYNC_EVERY
  while true do
    event.pull(0.25)
    readSocket()
    now = computer.uptime()
    if needResync or now >= nextResync then
      needResync = false
      fullSync()
      nextResync = now + RESYNC_EVERY
    end
    if now >= nextFlush then
      flushDeltas()
      pollJobs()
      nextFlush = now + FLUSH_EVERY
    end
    if now >= nextStatus then
      sendStatus()
      nextStatus = now + STATUS_EVERY
    end
  end
end

event.listen("network_item_changed", onItems)
event.listen("network_fluid_changed", onFluids)

local function interrupted(e)
  return tostring(e):find("interrupted", 1, true) ~= nil
end

local failure -- set when the script stops because of an error

while true do
  print("mecraft: подключаюсь к " .. HOST .. ":" .. PORT)
  local s, err = connect()
  local stop = false
  if s then
    sock = s
    local ok, e = pcall(session)
    if not ok then
      if interrupted(e) then
        stop = true
      elseif isFatal(e) then
        -- Out of memory and the like will not fix themselves: report and stop.
        failure = fatalText(e)
        pcall(send, { type = "fail", message = failure })
        stop = true
      else
        print("mecraft: " .. tostring(e))
      end
    end
    pcall(s.close)
    sock = nil
  else
    print("mecraft: не подключился: " .. tostring(err))
  end
  if stop or not pcall(os.sleep, 5) then
    break
  end
end

event.ignore("network_item_changed", onItems)
event.ignore("network_fluid_changed", onFluids)
pcall(me.setItemEventSubscription, false)
pcall(me.setFluidEventSubscription, false)
if failure then
  io.stderr:write("mecraft: ОШИБКА: " .. failure .. "\n")
  return 1
end
print("mecraft: остановлен")
