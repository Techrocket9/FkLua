-- 19 nested tables. The engine's own depth counter reads 20 here, at its limit.
local d = "leaf"
for i = 1, 19 do d = {"", d} end
data:extend({{type = "string-setting", name = "fkdp-loc", setting_type = "startup",
              default_value = "x", localised_description = d}})
