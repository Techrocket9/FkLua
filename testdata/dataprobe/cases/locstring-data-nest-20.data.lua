-- The nesting limit on an ordinary prototype at the DATA stage, so the claim
-- that the two stages do not differ is measured on both halves of the rule
-- rather than on the parameter half alone. 20 nested tables, which the engine
-- counts as 21.
local d = "leaf"
for i = 1, 20 do d = {"", d} end
local it = P.item("fkdp-loc-item")
it.localised_description = d
data:extend({it})
