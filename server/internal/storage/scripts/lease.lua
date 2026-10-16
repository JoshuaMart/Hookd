-- Takes or renews the eviction lease and returns 1 when this owner holds it.
-- ARGV: P, owner, ttl_ms
local k = P .. 'lock:evictor'
if redis.call('SET', k, ARGV[2], 'NX', 'PX', ARGV[3]) then return 1 end
if redis.call('GET', k) == ARGV[2] then
  redis.call('PEXPIRE', k, ARGV[3])
  return 1
end
return 0
