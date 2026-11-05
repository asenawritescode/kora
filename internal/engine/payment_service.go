package engine

import (
	"database/sql"

	"github.com/asenawritescode/kora/api"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/payment"
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
