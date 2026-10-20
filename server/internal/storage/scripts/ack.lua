-- Deletes the entries up to a seq and returns how many, or -1 for an unknown
-- hook or one out of scope.
-- ARGV: P, scope ('*' = any owner), id, end_id (<through>-0), min_id (<through+1>-0)
local id = ARGV[3]
local h, s = hkey(id), skey(id)
if not owned(h, ARGV[2]) then return -1 end

local gone = redis.call('XRANGE', s, '-', ARGV[4])
if #gone == 0 then return 0 end
uncount(h, gone)
redis.call('XTRIM', s, 'MINID', ARGV[5])
reindex(id, h, s)
return #gone
