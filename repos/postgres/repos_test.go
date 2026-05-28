//go:build postgres_integration

package postgres_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/bds421/rho-stripe/checkout"
	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/repos/postgres"
	"github.com/bds421/rho-stripe/repos/postgres/schema"
	_ "github.com/jackc/pgx/v5/stdlib" // sql.Open driver
)

// dsnEnvVar is read by setupDB to find a Postgres connection string.
// Defaults to a localhost test container.
const dsnEnvVar = "STRIPE_CONNECTOR_PG_DSN"

const defaultDSN = "postgres://postgres:scctest@localhost:5433/postgres?sslmode=disable"

func setupDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv(dsnEnvVar)
	if dsn == "" {
		dsn = defaultDSN
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("sql.Open: %v (set %s to a reachable test DB)", err, dsnEnvVar)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("ping: %v (postgres not reachable; skipping integration test)", err)
	}
	for _, ddl := range schema.All() {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("apply schema: %v", err)
		}
	}
	if _, err := db.Exec(`
		TRUNCATE TABLE stripe_connector_credit_deductions,
		               stripe_connector_credit_grants,
		               stripe_connector_customers,
		               stripe_connector_subscriptions,
		               stripe_connector_invoice_numbers,
		               stripe_connector_webhook_queue,
		               idempotency_keys
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return db
}

func TestCustomerRepo_RoundTrip(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewCustomerRepo(db)
	ctx := t.Context()

	if id, ok, err := repo.Get(ctx, "org_a"); err != nil || ok {
		t.Errorf("expected miss before upsert; got id=%q ok=%v err=%v", id, ok, err)
	}

	if err := repo.Upsert(ctx, "org_a", "cus_alpha"); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	id, ok, err := repo.Get(ctx, "org_a")
	if err != nil || !ok || id != "cus_alpha" {
		t.Errorf("Get after Upsert: id=%q ok=%v err=%v", id, ok, err)
	}

	// Updating an existing subject overwrites the customer id.
	if err := repo.Upsert(ctx, "org_a", "cus_beta"); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	id, _, _ = repo.Get(ctx, "org_a")
	if id != "cus_beta" {
		t.Errorf("after re-upsert id = %q, want cus_beta", id)
	}
}

func TestCreditRepo_GrantAndList(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewCreditRepo(db)
	ctx := t.Context()

	g, err := repo.Grant(ctx, credits.GrantInput{
		SubjectID: "org_acme", Bucket: "ai", Amount: 1000, ValidDays: 90,
		Source: credits.SourceStripePayment, SourceRef: "pi_abc:0:0",
		Metadata: map[string]string{"product_key": "credit_pack_1000_ai"},
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if g.ID == "" || g.AmountInitial != 1000 || g.AmountRemaining != 1000 {
		t.Errorf("Grant returned wrong shape: %+v", g)
	}
	if g.ExpiresAt == nil {
		t.Error("ExpiresAt should be set when ValidDays > 0")
	}

	list, err := repo.ListBySubject(ctx, "org_acme")
	if err != nil {
		t.Fatalf("ListBySubject: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("ListBySubject len = %d, want 1", len(list))
	}
	if list[0].Metadata["product_key"] != "credit_pack_1000_ai" {
		t.Errorf("metadata not roundtripped: %v", list[0].Metadata)
	}
}

func TestCreditRepo_GrantIdempotentOnSourceRef(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewCreditRepo(db)
	ctx := t.Context()

	in := credits.GrantInput{
		SubjectID: "org_x", Bucket: "ai", Amount: 500,
		Source: credits.SourceStripePayment, SourceRef: "pi_dup:0:0",
	}
	first, err := repo.Grant(ctx, in)
	if err != nil {
		t.Fatalf("first Grant: %v", err)
	}
	second, err := repo.Grant(ctx, in)
	if err != nil {
		t.Fatalf("second Grant: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("re-Grant created a new ledger row %q (want same as %q) — partial-unique-index dedup failed", second.ID, first.ID)
	}
	list, _ := repo.ListBySubject(ctx, "org_x")
	if len(list) != 1 {
		t.Errorf("expected 1 grant after dedup, got %d", len(list))
	}
}

func TestCreditRepo_AdminGrantsWithoutSourceRefCoexist(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := postgres.NewCreditRepo(db)
	ctx := t.Context()

	// Two admin grants with no SourceRef — partial UNIQUE only fires
	// when source_ref IS NOT NULL, so both should insert.
	for i := 0; i < 2; i++ {
		if _, err := repo.Grant(ctx, credits.GrantInput{
			SubjectID: "org_admin", Bucket: "ai", Amount: 100,
			Source: credits.SourceAdminGrant, // SourceRef intentionally empty
		}); err != nil {
			t.Fatalf("admin Grant %d: %v", i, err)
		}
	}
	list, _ := repo.ListBySubject(ctx, "org_admin")
	if len(list) != 2 {
		t.Errorf("expected 2 admin grants, got %d (partial-unique-index may be misconfigured)", len(list))
	}
}

func TestRepos_Bundle(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repos := postgres.NewRepos(db)

	if repos.Customers == nil || repos.Credits == nil || repos.Events == nil {
		t.Errorf("NewRepos returned nil field: %+v", repos)
	}

	// CustomerRepo path works through the bundle.
	if err := repos.Customers.Upsert(t.Context(), checkout.SubjectID("org_bundle"), "cus_b"); err != nil {
		t.Fatalf("bundled Customers.Upsert: %v", err)
	}
	if id, ok, _ := repos.Customers.Get(t.Context(), "org_bundle"); !ok || id != "cus_b" {
		t.Errorf("bundled Customers.Get: id=%q ok=%v", id, ok)
	}
}
