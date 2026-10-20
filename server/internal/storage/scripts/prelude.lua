-- Prepended to every script. ARGV[1] is always the key prefix.
--
-- Keys, all under the prefix:
--   h:<id>     HASH    hook fields, kind (eph|ll), owner, last_seq,
--                      dropped_through, c:<type> pending counts
--   i:<id>     STREAM  pending interactions, entry ID <seq>-0, fields t, ts, j
--   exp        ZSET    hooks with an expiry, scored by expires_at (µs)
--   ttl        ZSET    ephemeral hooks with pending interactions, scored by the
--                      oldest pending ts (µs)
--   ll         ZSET    long-lived hooks, scored by created_at (µs)
--   ll:active  ZSET    long-lived hooks with pending interactions, scored by
--                      the latest ts (µs)
--   o:<owner>  ZSET    long-lived hooks of an owner, scored by created_at (µs)
--   stats      HASH    hooks and per-type pending counts
--   evict:limit        insert-time limit evictions not yet reported
local P = ARGV[1]

local function hkey(id) return P .. 'h:' .. id end
local function skey(id) return P .. 'i:' .. id end

-- Reports whether the hook exists and is in scope: '*' matches any owner,
-- '' none. Unknown and foreign hooks cost the same single call.
local function owned(h, scope)
  if scope == '*' then return redis.call('EXISTS', h) == 1 end
  return scope ~= '' and redis.call('HGET', h, 'owner') == scope
end

-- Stream entries arrive as {id, {k1, v1, k2, v2, ...}}.
local function field(entry, name)
  local f = entry[2]
  for i = 1, #f, 2 do
    if f[i] == name then return f[i + 1] end
  end
end

local function seq_of(entry)
  return tonumber(string.match(entry[1], '^(%d+)'))
end

-- Callers must check the hook exists: HINCRBY would recreate its hash.
local function uncount(h, entries)
  for _, e in ipairs(entries) do
    local t = field(e, 't')
    redis.call('HINCRBY', h, 'c:' .. t, -1)
    redis.call('HINCRBY', P .. 'stats', t, -1)
  end
end

-- Records the highest seq removed before the client acknowledged it.
local function note_dropped(h, seq)
  if seq > tonumber(redis.call('HGET', h, 'dropped_through') or '0') then
    redis.call('HSET', h, 'dropped_through', seq)
  end
end

-- Keeps the pending indexes in step with the stream after a removal.
local function reindex(id, h, s)
  local head = redis.call('XRANGE', s, '-', '+', 'COUNT', 1)
  if #head == 0 then
    redis.call('ZREM', P .. 'ttl', id)
    redis.call('ZREM', P .. 'll:active', id)
  elseif redis.call('HGET', h, 'kind') ~= 'll' then
    redis.call('ZADD', P .. 'ttl', field(head[1], 'ts'), id)
  end
end

-- Drops the oldest entries beyond max and returns how many went.
local function trim_to(id, h, s, max)
  local over = redis.call('XLEN', s) - max
  if over <= 0 then return 0 end
  local drop = redis.call('XRANGE', s, '-', '+', 'COUNT', over)
  uncount(h, drop)
  local last = seq_of(drop[#drop])
  redis.call('XTRIM', s, 'MINID', (last + 1) .. '-0')
  note_dropped(h, last)
  reindex(id, h, s)
  return #drop
end
