-- The same limit on an ordinary prototype at the DATA stage, to show the two
-- stages do not differ: same message, different prototype named in front of it.
local d = {""}
for i = 1, 21 do d[#d + 1] = "p" .. i end
local it = P.item("fkdp-loc-item")
it.localised_description = d
data:extend({it})
