-- Returns a hook's fields, or nil for an unknown hook or one out of scope.
-- ARGV: P, scope ('*' = any owner), id
local h = hkey(ARGV[3])
if not owned(h, ARGV[2]) then return false end
return redis.call('HGETALL', h)
