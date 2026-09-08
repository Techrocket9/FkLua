-- Prelude, prepended to BOTH settings.lua and data.lua of every case.
--
-- It only DEFINES things. A case file is the body that follows, so a case is
-- the few lines that are the fact and nothing else, and two cases differing in
-- one number differ in one line here.
--
-- The three builders are the smallest prototypes the engine accepts, and they
-- borrow base's own art rather than shipping any: a probe that had to carry a
-- PNG could not be a directory of text files.
--
-- `local`, and reachable from the case body anyway, because the two are
-- concatenated into ONE chunk. Nothing here reaches the global environment.
local P = {}

-- P.item builds a minimal item prototype.
function P.item(name)
  return {type = "item", name = name, icon = "__base__/graphics/icons/iron-plate.png", stack_size = 10}
end

-- P.recipe builds a minimal recipe. A nil category is the engine's default,
-- which is 'crafting' and which is the one that refuses a fluid.
function P.recipe(name, ingredients, category)
  return {type = "recipe", name = name, category = category, ingredients = ingredients,
          results = {{type = "item", name = "iron-plate", amount = 1}}}
end

-- P.tech builds a minimal technology. `unit` is what most of the research
-- cases vary.
function P.tech(name, unit)
  return {type = "technology", name = name, icon = "__base__/graphics/technology/automation-1.png",
          icon_size = 256, effects = {}, unit = unit}
end
