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

local BATCH = 200          -- stacks per message
local FLUSH_EVERY = 0.5    -- seconds between sending accumulated changes
local STATUS_EVERY = 5     -- seconds between CPU/power reports
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

local function send(msg)
  local data = serialization.serialize(msg) .. "\n"
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

  local batch = {}
  for _, v in pairs(items) do
    if v.size ~= 0 then
      batch[#batch + 1] = v
      if #batch >= BATCH then
        send({ type = "item_delta", items = batch })
        batch = {}
      end
    end
  end
  if #batch > 0 then
    send({ type = "item_delta", items = batch })
  end

  local fl = {}
  for _, v in pairs(fluids) do
    if v.amount ~= 0 then
      fl[#fl + 1] = v
    end
  end
  if #fl > 0 then
    send({ type = "fluid_delta", fluids = fl })
  end
end

------------------------------------------------------------------------
-- full state

local function fullSync()
  itemDelta, fluidDelta = {}, {}
  local items = me.getItemsInNetwork()
  send({ type = "items_begin" })
  local batch = {}
  for i = 1, count(items) do
    batch[#batch + 1] = itemOf(items[i])
    if #batch >= BATCH then
      send({ type = "items", items = batch })
      batch = {}
    end
  end
  if #batch > 0 then
    send({ type = "items", items = batch })
  end
  items = nil
  send({ type = "items_end" })

  local fluids = me.getFluidsInNetwork()
  local out = {}
  for i = 1, count(fluids) do
    out[#out + 1] = fluidOf(fluids[i])
  end
  send({ type = "fluids", fluids = out })
end

local function sendStatus()
  local cpus = me.getCpus()
  local out = {}
  for i = 1, count(cpus) do
    local c = cpus[i]
    out[#out + 1] = {
      name = c.name or "",
      storage = c.storage or 0,
      coprocessors = c.coprocessors or 0,
      busy = truthy(c.busy),
    }
  end
  send({ type = "cpus", cpus = out })
  send({
    type = "power",
    stored = me.getStoredPower(),
    max = me.getMaxStoredPower(),
    usage = me.getAvgPowerUsage(),
    injection = me.getAvgPowerInjection(),
  })
end

------------------------------------------------------------------------
-- crafting orders

local jobs = {}

local function craft(msg)
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
  if not pick then
    send({ type = "job", job = msg.job, state = "failed", reason = "нет рецепта" })
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

while true do
  print("mecraft: подключаюсь к " .. HOST .. ":" .. PORT)
  local s, err = connect()
  local stop = false
  if s then
    sock = s
    local ok, e = pcall(session)
    pcall(s.close)
    sock = nil
    if not ok then
      if interrupted(e) then
        stop = true
      else
        print("mecraft: " .. tostring(e))
      end
    end
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
print("mecraft: остановлен")
