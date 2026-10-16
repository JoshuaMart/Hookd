-- Drops ephemeral interactions captured before cutoff, for up to limit hooks.
-- Returns {hooks visited, interactions removed}.
-- ARGV: P, cutoff_us, limit
local cutoff = tonumber(ARGV[2])
local ids = redis.call('ZRANGE', P .. 'ttl', '-inf', '(' .. ARGV[2], 'BYSCORE', 'LIMIT', 0, ARGV[3])
local total = 0
for _, id in ipairs(ids) do
  local h, s = hkey(id), skey(id)
  if redis.call('EXISTS', h) == 0 then
    redis.call('ZREM', P .. 'ttl', id)
  else
    -- Stream order is capture order, which matches capture time closely
    -- enough for an hour-scale TTL: stop at the first entry still inside it.
    local drop, start, done = {}, '-', false
    repeat
      local page = redis.call('XRANGE', s, start, '+', 'COUNT', 64)
      for _, e in ipairs(page) do
        if tonumber(field(e, 'ts')) >= cutoff then
          done = true
          break
        end
        drop[#drop + 1] = e
      end
      if #page < 64 then done = true end
      if not done then start = '(' .. page[#page][1] end
    until done
    if #drop > 0 then
      uncount(h, drop)
      local last = seq_of(drop[#drop])
      redis.call('XTRIM', s, 'MINID', (last + 1) .. '-0')
      note_dropped(h, last)
      total = total + #drop
    end
    reindex(id, h, s)
  end
end
return { #ids, total }
