-- Deletes up to limit hooks that expired before now and returns how many.
-- ARGV: P, now_us, limit
local ids = redis.call('ZRANGE', P .. 'exp', '-inf', '(' .. ARGV[2], 'BYSCORE', 'LIMIT', 0, ARGV[3])
for _, id in ipairs(ids) do
  local h = hkey(id)
  -- The hash is gone when its safety-net expiry fired first.
  if redis.call('EXISTS', h) == 1 then
    for _, t in ipairs({ 'dns', 'http', 'smtp' }) do
      local c = tonumber(redis.call('HGET', h, 'c:' .. t) or '0')
      if c ~= 0 then redis.call('HINCRBY', P .. 'stats', t, -c) end
    end
    redis.call('HINCRBY', P .. 'stats', 'hooks', -1)
  end
  redis.call('DEL', h, skey(id))
  redis.call('ZREM', P .. 'exp', id)
  redis.call('ZREM', P .. 'ttl', id)
  redis.call('ZREM', P .. 'll', id)
  redis.call('ZREM', P .. 'll:active', id)
end
return #ids
