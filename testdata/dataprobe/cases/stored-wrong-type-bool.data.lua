-- The same reset for a bool, to show the rule is about the type and not about
-- one wrong type.
local v = settings.startup["fkdp-text"].value
log("PROBE wrong-typed stored value reads back as [" .. tostring(v) .. "]")
if v ~= "default" then error("a bool under a string setting did not reset to the default: " .. tostring(v)) end
