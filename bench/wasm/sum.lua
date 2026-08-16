-- Driver for sum.wat. M is the instantiated module.
local W, R = 65536, 160
M.exports["setup"](W)
local acc = 0
for _ = 1, R do acc = (acc + M.exports["kernel"](0, W)) % 4294967296.0 end
print(string.format("%.0f\t%d", acc, R * W))
