-- Driver for count.wat. M is the instantiated module.
local N = 4000000
print(string.format("%.0f\t%d", M.exports["kernel"](N), N))
