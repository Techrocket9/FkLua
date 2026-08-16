local N, R = 200000, 12
local acc = 0.0
for _ = 1, R do acc = acc + M.exports["kernel"](N) end
print(string.format("%.6f\t%d", acc, R * N))
