package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsRetryable(t *testing.T) {
	for code, want := range map[string]bool{"40P01": true, "40001": true, "23505": false, "57014": false} {
		err := fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code})
		if got := IsRetryable(err); got != want {
			t.Errorf("IsRetryable(%s) = %v, want %v", code, got, want)
		}
	}
	if IsRetryable(errors.New("plain")) || IsRetryable(nil) {
		t.Error("a non-Postgres error is not retryable")
	}
}

func TestTxRetry(t *testing.T) {
	url := os.Getenv("OPENDESK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set OPENDESK_TEST_DATABASE_URL to run database tests")
	}
	ctx := context.Background()
	d, err := Open(ctx, url, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// A deadlock is run again, and the run that succeeds commits.
	runs := 0
	err = d.TxRetry(ctx, func(tx pgx.Tx) error {
		runs++
		if runs == 1 {
			return &pgconn.PgError{Code: "40P01"}
		}
		return nil
	})
	if err != nil || runs != 2 {
		t.Fatalf("after a deadlock: err %v, %d runs", err, runs)
	}

	// The attempts are bounded.
	runs = 0
	err = d.TxRetry(ctx, func(tx pgx.Tx) error {
		runs++
		return &pgconn.PgError{Code: "40001"}
	})
	if !IsRetryable(err) || runs != txAttempts {
		t.Fatalf("always failing: err %v, %d runs", err, runs)
	}

	// Any other error is returned at once.
	runs = 0
	err = d.TxRetry(ctx, func(tx pgx.Tx) error {
		runs++
		return &pgconn.PgError{Code: "23505"}
	})
	if err == nil || runs != 1 {
		t.Fatalf("unique violation: err %v, %d runs", err, runs)
	}

	// Tx itself runs once.
	runs = 0
	_ = d.Tx(ctx, func(tx pgx.Tx) error {
		runs++
		return &pgconn.PgError{Code: "40P01"}
	})
	if runs != 1 {
		t.Fatalf("Tx ran %d times", runs)
	}
}
