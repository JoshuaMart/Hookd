-- Trims each listed hook to max entries and returns how many were dropped.
-- ARGV: P, max, id...
local max, total = tonumber(ARGV[2]), 0
for i = 3, #ARGV do
  local id = ARGV[i]
  local h = hkey(id)
  if redis.call('EXISTS', h) == 1 then
    total = total + trim_to(id, h, skey(id), max)
  end
end
return total
