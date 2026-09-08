-- A setting PROTOTYPE is not in data.raw at the data stage, while its VALUE is
-- in settings.startup. So anything that needs to know what settings a mod
-- declared has to carry the declarations itself.
if data.raw["string-setting"] ~= nil then
  error('data.raw["string-setting"] is not nil at the data stage')
end
log('PROBE data.raw["string-setting"] is nil at the data stage')
log("PROBE settings.startup fkdp-text = " .. tostring(settings.startup["fkdp-text"].value))
