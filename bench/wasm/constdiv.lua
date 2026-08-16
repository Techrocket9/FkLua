-- Driver for constdiv.wat. M is the instantiated module.
--
-- Ops is the iteration count, not the division count: five divisions per
-- iteration would make the ns/op figure incomparable with every other kernel
-- here, and the ratio between levels -- which is the only number this kernel
-- exists to produce -- is the same either way.
--
-- The checksum is what makes the comparison honest. -opt=0 computes it through
-- div_u/rem_u and -opt>=1 through native arithmetic, so the harness refusing a
-- run whose checksums disagree is a real correctness gate on this lowering and
-- not ceremony.
local N = 2000000
print(string.format("%.0f\t%d", M.exports["kernel"](N), N))
