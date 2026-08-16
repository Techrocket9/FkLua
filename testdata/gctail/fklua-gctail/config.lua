-- The default arm. scripts/run-gctail.sh OVERWRITES this file per arm, so the
-- values here are only what the mod does if somebody runs it by hand.
--
-- shards x shardw is the live set, in words. 26 x 2^19 is 52 MiB, which is the
-- size agents/guests.md's cost table has two rows for that disagree by 3.7x.
return {
  shards = 26,
  shardw = 524288,
  churn = 16,
  churnw = 64,
  pace = 0,
}
