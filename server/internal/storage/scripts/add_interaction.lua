-- Stores an interaction and returns its seq, or 0 when the hook is unknown.
-- ARGV: P, id, max_per_hook (0 = no limit), ts_us, type, json,
--   json_ll (the body-truncated form for long-lived hooks, '' when identical)
local id, max, ts, t = ARGV[2], tonumber(ARGV[3]), ARGV[4], ARGV[5]
local h, s = hkey(id), skey(id)
if redis.call('EXISTS', h) == 0 then return 0 end

local ll = redis.call('HGET', h, 'kind') == 'll'
local j = ARGV[6]
if ll and ARGV[7] ~= '' then j = ARGV[7] end

local seq = redis.call('HINCRBY', h, 'last_seq', 1)
redis.call('XADD', s, seq .. '-0', 't', t, 'ts', ts, 'j', j)
redis.call('HINCRBY', h, 'c:' .. t, 1)
redis.call('HINCRBY', P .. 'stats', t, 1)

if ll then
  redis.call('ZADD', P .. 'll:active', 'GT', ts, id)
else
  redis.call('ZADD', P .. 'ttl', 'NX', ts, id)
end
local at = redis.call('PEXPIRETIME', h)
if at > 0 then redis.call('PEXPIREAT', s, at) end

if max > 0 then
  local n = trim_to(id, h, s, max)
  if n > 0 then redis.call('INCRBY', P .. 'evict:limit', n) end
end
return seq
