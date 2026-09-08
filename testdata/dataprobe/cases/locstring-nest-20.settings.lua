-- 20 nested tables, which the engine counts as 21.
local d = "leaf"
for i = 1, 20 do d = {"", d} end
data:extend({{type = "string-setting", name = "fkdp-loc", setting_type = "startup",
              default_value = "x", localised_description = d}})
