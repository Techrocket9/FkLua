-- THE ABORT BOUNDARY, from below. `p` in the case name is the decimal point:
-- 1.0715e301. This loads, and the next case is 1.0716e301, which does not.
-- Between them sits DBL_MAX / 2^24 = 1.0715086071862672e301, so the fixed-point
-- conversion scales by 2^24 and the boundary is where that multiplication
-- overflows a double to infinity. Bracketing it is what makes the constant a
-- measurement rather than an arithmetic guess.
data:extend({P.recipe("fkdp-r", {{type = "fluid", name = "water", amount = 1.0715e301}}, "crafting-with-fluid")})
