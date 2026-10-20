-- Drains several hooks. Returns one reply per id: nil for an unknown hook or
-- one out of scope, else its entries. ARGV: P, scope ('*' = any owner), id...
local out = {}
for i = 3, #ARGV do
  local id = ARGV[i]
  local h, s = hkey(id), skey(id)
  if not owned(h, ARGV[2]) then
    out[#out + 1] = false
  else
    local entries = redis.call('XRANGE', s, '-', '+')
    if #entries > 0 then
      uncount(h, entries)
      redis.call('DEL', s)
      redis.call('ZREM', P .. 'ttl', id)
      redis.call('ZREM', P .. 'll:active', id)
    end
    out[#out + 1] = entries
  end
end
return out
