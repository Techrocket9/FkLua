-- One parameter past the limit. The engine counts the key separately.
local d = {""}
for i = 1, 21 do d[#d + 1] = "p" .. i end
data:extend({{type = "string-setting", name = "fkdp-loc", setting_type = "startup",
              default_value = "x", localised_description = d}})
