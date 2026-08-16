local N = 30
-- fib(n) makes 2*fib(n+1)-1 calls; counting them exactly keeps ns/op honest.
local a, b = 1, 1
for _ = 2, N do a, b = b, a + b end
print(string.format("%.0f\t%d", M.exports["kernel"](N), 2 * (a + b) - 1))
