-- The same fact from the other side: indexing into the absent table is an
-- ordinary Lua error, so the mistake stops the load rather than reading nil.
local _ = data.raw["string-setting"]["fkdp-text"]
