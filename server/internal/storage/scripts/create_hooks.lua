-- Creates a batch of hooks, all or nothing, under the long-lived cap.
-- ARGV: P, kind, max_hooks (0 = no cap), n, then per hook:
--   id, created_ns, expires_ns, created_us, expires_us (0 = none),
--   expire_at_ms, dns, http, https, smtp, metadata
local kind, max, n = ARGV[2], tonumber(ARGV[3]), tonumber(ARGV[4])
local W = 11

if kind == 'll' and max > 0 and redis.call('ZCARD', P .. 'll') + n > max then
  return redis.error_reply('HOOKD_LIMIT')
end
for k = 0, n - 1 do
  if redis.call('EXISTS', hkey(ARGV[5 + k * W])) == 1 then
    return redis.error_reply('HOOKD_COLLISION')
  end
end

for k = 0, n - 1 do
  local b = 4 + k * W
  local id = ARGV[b + 1]
  local h = hkey(id)
  redis.call('HSET', h,
    'created_at', ARGV[b + 2], 'expires_at', ARGV[b + 3],
    'dns', ARGV[b + 7], 'http', ARGV[b + 8], 'https', ARGV[b + 9], 'smtp', ARGV[b + 10],
    'metadata', ARGV[b + 11], 'kind', kind, 'last_seq', 0, 'dropped_through', 0)
  if ARGV[b + 5] ~= '0' then
    redis.call('ZADD', P .. 'exp', ARGV[b + 5], id)
    -- Safety net: the evictor normally deletes the hook first.
    redis.call('PEXPIREAT', h, ARGV[b + 6])
  end
  if kind == 'll' then
    redis.call('ZADD', P .. 'll', ARGV[b + 4], id)
  end
end
redis.call('HINCRBY', P .. 'stats', 'hooks', n)
return n
