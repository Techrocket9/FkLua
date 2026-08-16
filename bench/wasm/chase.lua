local N, R = 32768, 60
M.exports["setup"](N)
local acc = 0
for _ = 1, R do acc = (acc + M.exports["kernel"](0, N)) % 4294967296.0 end
print(string.format("%.0f\t%d", acc, R * N))
