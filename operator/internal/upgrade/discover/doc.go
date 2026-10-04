// Package discover fills the graph the checks and the steps read. §17 of
// design-api-upgrade.md requires the migration to list every resource relevant
// to it and build an explicit graph, rather than processing objects as it
// encounters them, because the ownership edges it is about to move are only
// safe to move once every dependent of an owner is known.
//
// One discoverer per kind, declared as data. Adding a kind to the migration is
// adding a [Kind] to the list in catalog, and every check written before it
// sees the new objects without being changed.
package discover
