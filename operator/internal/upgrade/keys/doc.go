// Package keys is the inventory of annotation and label keys that move from the
// bare simplyblock.io prefix to storage.simplyblock.io, and the matching that
// goes with it.
//
// §16.3 of design-api-upgrade.md calls this the quietest break in the whole
// migration. No API server rejects the old key, so a PersistentVolumeClaim
// annotated simplyblock.io/backup-policy simply stops having a backup policy,
// and nothing anywhere reports that it used to.
//
// The inventory is a package of its own rather than a table inside the check
// that reads it, because two things read it. The preflight asks whether any
// object carries both spellings with two different values, which is the one
// state the rewrite cannot resolve on the user's behalf, and the migration
// rewrites every key it finds.
package keys
