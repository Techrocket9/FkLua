-- The settings stage sees an EMPTY data.raw. agents/datastage.md's stage
-- environment table records the type count of 0; this case is the re-measurable
-- form of it, and it is what makes a picker over the game's items impossible in
-- a setting.
local n = 0
for _ in pairs(data.raw) do n = n + 1 end
log("PROBE data.raw type count at the settings stage: " .. n)
if n ~= 0 then error("data.raw is not empty at the settings stage: " .. n .. " types") end
if data.raw.item ~= nil or data.raw.recipe ~= nil then
  error("data.raw.item or data.raw.recipe exists at the settings stage")
end
