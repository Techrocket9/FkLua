-- The composition a description with many entries has to use: 19 parameters
-- plus a nested group carrying 20 more. Both levels are at the limit and the
-- limits do not add up, so nesting is how a long description is built.
local inner = {""}
for i = 1, 20 do inner[#inner + 1] = "q" .. i end
local d = {""}
for i = 1, 19 do d[#d + 1] = "p" .. i end
d[#d + 1] = inner
data:extend({{type = "string-setting", name = "fkdp-loc", setting_type = "startup",
              default_value = "x", localised_description = d}})
