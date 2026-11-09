package engine

import (
	"database/sql"

	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/payment"
)

// newPaymentService builds the per-site payment service: SQL store over the
// site database, with cash registered by default.
func newPaymentService(database *sql.DB, dialect db.Dialect) *payment.Service {
	return &payment.Service{
		Store:     &payment.SQLStore{DB: database, Dialect: dialect},
		Providers: payment.NewRegistry(),
	}
}

func init() { api.PaymentServiceFactory = newPaymentService }
