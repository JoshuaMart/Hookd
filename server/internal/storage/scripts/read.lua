-- Returns {dropped_through, entries past the cursor}, or nil for an unknown
-- hook or one of another owner.
-- ARGV: P, owner ('' = any), id, start_id (the first stream ID past the cursor)
local h = hkey(ARGV[3])
if not owned(h, ARGV[2]) then return false end
return { redis.call('HGET', h, 'dropped_through'), redis.call('XRANGE', skey(ARGV[3]), ARGV[4], '+') }
