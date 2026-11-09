# DEV-23

**Mutation.** Two multipath LUNs, each over two iSCSI paths, block run

**Expected.** Both maps. Each path is declined by `available` as held by its map, and nothing else on the worker displaces the maps

**Harness.** `CM`
