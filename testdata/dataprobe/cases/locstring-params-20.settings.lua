-- 20 parameters under one key: the most a localised string may carry.
local d = {""}
for i = 1, 20 do d[#d + 1] = "p" .. i end
data:extend({{type = "string-setting", name = "fkdp-loc", setting_type = "startup",
              default_value = "x", localised_description = d}})
