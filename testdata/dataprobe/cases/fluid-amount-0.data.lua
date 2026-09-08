-- A fluid amount of zero. A different message from the item one: a fluid
-- amount is a fixed-point double, not a u16.
data:extend({P.recipe("fkdp-r", {{type = "fluid", name = "water", amount = 0}}, "crafting-with-fluid")})
