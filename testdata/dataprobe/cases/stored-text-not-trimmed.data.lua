-- auto_trim is a GUI behaviour and nothing else. A value already in the file
-- reaches the stage with both space runs intact, so anything reading a text
-- setting trims for itself.
local want = "  3 iron-plate , 0.5 [fluid=water]  "
local v = settings.startup["fkdp-text"].value
log("PROBE stored text reads back as [" .. v .. "] length " .. #v)
if v ~= want then error("auto_trim changed the stored text: [" .. v .. "]") end
