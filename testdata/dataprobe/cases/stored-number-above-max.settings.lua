data:extend({{type = "int-setting", name = "fkdp-int", setting_type = "startup",
              default_value = 1, minimum_value = 0, maximum_value = 65535}})
-- THE WITNESS. The fact under test is that the stored value reads back as the
-- DEFAULT, which is also what an unread mod-settings.dat gives, so without a
-- second setting whose stored value differs from its default this case would
-- pass on a file nothing proved was read.
data:extend({{type = "string-setting", name = "fkdp-witness", setting_type = "startup",
              default_value = "absent"}})
