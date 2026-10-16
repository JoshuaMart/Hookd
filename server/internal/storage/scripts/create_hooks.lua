-- Creates a batch of hooks, all or nothing, under the long-lived cap.
-- ARGV: P, max_long_lived (0 = no cap), n, then per hook:
--   kind (eph|ll), id, created_ns, expires_ns, created_us,
--   expires_us (0 = none), expire_at_ms, dns, http, https, smtp, metadata
local max, n = tonumber(ARGV[2]), tonumber(ARGV[3])
local W = 12

local ll = 0
for k = 0, n - 1 do
  local b = 3 + k * W
  if ARGV[b + 1] == 'll' then ll = ll + 1 end
  if redis.call('EXISTS', hkey(ARGV[b + 2])) == 1 then
    return redis.error_reply('HOOKD_COLLISION')
  end
end
if ll > 0 and max > 0 and redis.call('ZCARD', P .. 'll') + ll > max then
  return redis.error_reply('HOOKD_LIMIT')
end

for k = 0, n - 1 do
  local b = 3 + k * W
  local kind, id = ARGV[b + 1], ARGV[b + 2]
  local h = hkey(id)
  redis.call('HSET', h,
    'created_at', ARGV[b + 3], 'expires_at', ARGV[b + 4],
    'dns', ARGV[b + 8], 'http', ARGV[b + 9], 'https', ARGV[b + 10], 'smtp', ARGV[b + 11],
    'metadata', ARGV[b + 12], 'kind', kind, 'last_seq', 0, 'dropped_through', 0)
  if ARGV[b + 6] ~= '0' then
    redis.call('ZADD', P .. 'exp', ARGV[b + 6], id)
    -- Safety net: the evictor normally deletes the hook first.
    redis.call('PEXPIREAT', h, ARGV[b + 7])
  end
  if kind == 'll' then
    redis.call('ZADD', P .. 'll', ARGV[b + 5], id)
  end
end
redis.call('HINCRBY', P .. 'stats', 'hooks', n)
return n
