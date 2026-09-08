-- ...and the same below the minimum.
local w = settings.startup["fkdp-witness"].value
if w ~= "present" then
  error("fkdp-witness reads back as [" .. tostring(w) .. "], not [present]: the stored settings file was not read at all, so this case measures nothing")
end
local v = settings.startup["fkdp-dbl"].value
log("PROBE stored 0.1 below minimum_value 0.5 reads back as " .. tostring(v))
if v ~= 1.0 then error("0.1 was not reset to the default 1.0: " .. tostring(v)) end
