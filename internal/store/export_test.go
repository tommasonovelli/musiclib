package store

// DB exposes the connection of q to the external tests, which run raw SQL
// inside the runner's transactions. It exists only in test builds.
func DB(q *Queries) DBTX { return q.db }

// CatalogQueries exposes the *Queries inside tx to the external tests, which
// run raw SQL under the catalog lock. It exists only in test builds: outside
// them, the field is unreachable from another package (N-095).
func CatalogQueries(tx *CatalogTx) *Queries { return tx.queries }
