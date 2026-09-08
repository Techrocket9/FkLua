-- A stored number outside the declared range is RESET to the default, not
-- clamped to the bound.
local w = settings.startup["fkdp-witness"].value
if w ~= "present" then
  error("fkdp-witness reads back as [" .. tostring(w) .. "], not [present]: the stored settings file was not read at all, so this case measures nothing")
end
local v = settings.startup["fkdp-int"].value
log("PROBE stored 70000 above maximum_value 65535 reads back as " .. tostring(v))
if v ~= 1 then error("70000 was not reset to the default 1: " .. tostring(v)) end
