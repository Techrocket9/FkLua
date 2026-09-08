-- A unit with NO packs loads. Whether such a research completes in a running
-- game is not something --dump-data can answer; see agents/datastage.md.
data:extend({P.tech("fkdp-t", {count = 10, time = 5, ingredients = {}})})
