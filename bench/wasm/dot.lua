local N, R = 16384, 80
M.exports["setup"](N)
local acc = 0.0
for _ = 1, R do acc = acc + M.exports["kernel"](0, N * 8, N) end
print(string.format("%.6f\t%d", acc, R * N))
