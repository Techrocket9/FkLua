data:extend({{type = "string-setting", name = "fkdp-drop", setting_type = "startup",
              default_value = "fast", allowed_values = {"fast", "slow"}}})
-- THE WITNESS, for the same reason as the two range cases: the value under
-- test reads back as its default, which an unread mod-settings.dat also gives.
data:extend({{type = "string-setting", name = "fkdp-witness", setting_type = "startup",
              default_value = "absent"}})
