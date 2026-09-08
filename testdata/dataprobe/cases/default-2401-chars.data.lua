-- A long declared default survives the settings stage and reads back whole.
local v = settings.startup["fkdp-text"].value
log("PROBE default length read back at the data stage: " .. #v)
if #v ~= 2401 then error("the 2401-character default read back at length " .. #v) end
