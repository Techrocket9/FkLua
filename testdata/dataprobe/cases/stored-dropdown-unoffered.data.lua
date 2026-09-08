-- A stored dropdown value the setting does not offer is reset to the default
-- before any stage runs, so a stage never sees an unoffered value from the
-- engine. It can still see one from a mod that changed its allowed_values, and
-- that is a different question.
local w = settings.startup["fkdp-witness"].value
if w ~= "present" then
  error("fkdp-witness reads back as [" .. tostring(w) .. "], not [present]: the stored settings file was not read at all, so this case measures nothing")
end
local v = settings.startup["fkdp-drop"].value
log("PROBE stored value outside allowed_values reads back as [" .. tostring(v) .. "]")
if v ~= "fast" then error("an unoffered dropdown value was not reset to the default: " .. tostring(v)) end
