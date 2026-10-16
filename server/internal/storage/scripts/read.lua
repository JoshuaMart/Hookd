-- Returns {dropped_through, entries past the cursor}, or nil for an unknown hook.
-- ARGV: P, id, start_id (the first stream ID past the cursor)
local h = hkey(ARGV[2])
if redis.call('EXISTS', h) == 0 then return false end
return { redis.call('HGET', h, 'dropped_through'), redis.call('XRANGE', skey(ARGV[2]), ARGV[3], '+') }
