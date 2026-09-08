data:extend({{type = "double-setting", name = "fkdp-dbl", setting_type = "startup",
              default_value = 1.0, minimum_value = 0.5, maximum_value = 100.0}})
-- THE WITNESS, for the same reason as the case above: the value under test
-- reads back as its default, which an unread mod-settings.dat also gives.
data:extend({{type = "string-setting", name = "fkdp-witness", setting_type = "startup",
              default_value = "absent"}})
