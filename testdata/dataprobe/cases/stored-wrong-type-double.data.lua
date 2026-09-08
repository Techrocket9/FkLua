-- A stored value of the wrong type is RESET to the default and the load
-- continues: the engine logs one Error line and still exits 0 with a dump. So
-- an Error line in the output is not by itself a refusal, which is why this
-- probe classifies on the exit code and the dump first.
local v = settings.startup["fkdp-text"].value
log("PROBE wrong-typed stored value reads back as [" .. tostring(v) .. "]")
if v ~= "default" then error("a double under a string setting did not reset to the default: " .. tostring(v)) end
