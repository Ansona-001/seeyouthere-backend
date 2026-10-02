package httpapi

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// inTx runs fn inside a transaction, committing on a nil return and rolling
// back otherwise. fn gets both the raw pgx.Tx (needed to enqueue a River
// job with InsertTx in the same transaction) and a *store.Queries bound to
// it.
func (s *Server) inTx(ctx context.Context, fn func(tx pgx.Tx, q *store.Queries) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return fn(tx, s.q.WithTx(tx))
	})
}
