package maintenance

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/store"
)

// CodeSchema refuses doctor, rebuild and backup on a database that is not at
// the binary's latest embedded migration (NOTES.md N-225, N-230 D4).
const CodeSchema = "maintenance_schema"

// RequireCurrentSchema reads goose's version without applying migrations:
// offline inspection and regeneration never migrate, the server's boot does.
// Restore is the only offline operation that applies forward migrations.
func RequireCurrentSchema(ctx context.Context, db *pgxpool.Pool) error {
	latest, err := store.LatestSchemaVersion()
	if err != nil {
		return fail(CodeSchema, "cannot read the embedded migrations", err)
	}
	version, err := store.SchemaVersion(ctx, db)
	if err != nil {
		return &Error{Code: CodeSchema, Message: "the database has no readable schema version; start the app once to apply supported migrations", Refusal: true, Err: err}
	}
	if version != latest {
		return refuse(CodeSchema, "the database schema is not the current one; start the app once to apply supported migrations")
	}
	return nil
}
