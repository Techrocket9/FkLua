-- THE ONE CASE THAT DOES NOT REFUSE. A fluid amount the fixed-point conversion
-- cannot hold ABORTS the process through the crash handler, so a library that
-- lets one through hands a player a crash dialog instead of a message. This is
-- why the probe classifies an abort separately from a refusal.
data:extend({P.recipe("fkdp-r", {{type = "fluid", name = "water", amount = 1e302}}, "crafting-with-fluid")})
