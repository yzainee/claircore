package postgres

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/quay/zlog"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/label"

	"github.com/quay/claircore/libvuln/driver"
	"github.com/quay/claircore/pkg/microbatch"
)

var (
	updateEnrichmentsCounter = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "claircore",
			Subsystem: "vulnstore",
			Name:      "updateenrichments_total",
			Help:      "Total number of database queries issued in the UpdateEnrichments method.",
		},
		[]string{"query"},
	)
	updateEnrichmentsDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "claircore",
			Subsystem: "vulnstore",
			Name:      "updateenrichments_duration_seconds",
			Help:      "The duration of all queries issued in the UpdateEnrichments method",
		},
		[]string{"query"},
	)
	getEnrichmentsCounter = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "claircore",
			Subsystem: "vulnstore",
			Name:      "getenrichments_total",
			Help:      "Total number of database queries issued in the get method.",
		},
		[]string{"query"},
	)
	getEnrichmentsDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "claircore",
			Subsystem: "vulnstore",
			Name:      "getenrichments_duration_seconds",
			Help:      "The duration of all queries issued in the get method",
		},
		[]string{"query"},
	)
)

// UpdateEnrichments creates a new UpdateOperation, inserts the provided
// EnrichmentRecord(s), and ensures enrichments from previous updates are not
// queried by clients.
func (s *Store) UpdateEnrichments(ctx context.Context, name string, fp driver.Fingerprint, es []driver.EnrichmentRecord) (uuid.UUID, error) {
	const (
		create = `
INSERT
INTO
	update_operation (updater, fingerprint, kind)
VALUES
	($1, $2, 'enrichment')
RETURNING
	id, ref;`
		insert = `
INSERT
INTO
	enrichment (hash_kind, hash, updater, tags, data)
VALUES
	($1, $2, $3, $4, $5)
ON CONFLICT
	(hash_kind, hash)
DO
	NOTHING;`
		assoc = `
INSERT
INTO
	uo_enrich (enrich, updater, uo, date)
VALUES
	(
		(
			SELECT
				id
			FROM
				enrichment
			WHERE
				hash_kind = $1
				AND hash = $2
				AND updater = $3
		),
		$3,
		$4,
		transaction_timestamp()
	)
ON CONFLICT
DO
	NOTHING;`
	)
	ctx = baggage.ContextWithValues(ctx,
		label.String("component", "internal/vulnstore/postgres/UpdateEnrichments"))

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("unable to start transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var id uint64
	var ref uuid.UUID

	start := time.Now()

	if err := s.pool.QueryRow(ctx, create, name, string(fp)).Scan(&id, &ref); err != nil {
		return uuid.Nil, fmt.Errorf("failed to create update_operation: %w", err)
	}

	updateEnrichmentsCounter.WithLabelValues("create").Add(1)
	updateEnrichmentsDuration.WithLabelValues("create").Observe(time.Since(start).Seconds())

	zlog.Debug(ctx).
		Str("ref", ref.String()).
		Msg("update_operation created")

	batch := microbatch.NewInsert(tx, 2000, time.Minute)
	start = time.Now()
	for i := range es {
		hashKind, hash := hashEnrichment(&es[i])
		err := batch.Queue(ctx, insert,
			hashKind, hash, name, es[i].Tags, es[i].Enrichment,
		)
		if err != nil {
			return uuid.Nil, fmt.Errorf("failed to queue enrichment: %w", err)
		}
		if err := batch.Queue(ctx, assoc, hashKind, hash, name, id); err != nil {
			return uuid.Nil, fmt.Errorf("failed to queue association: %w", err)
		}
	}
	if err := batch.Done(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("failed to finish batch enrichment insert: %w", err)
	}
	updateEnrichmentsCounter.WithLabelValues("insert_batch").Add(1)
	updateEnrichmentsDuration.WithLabelValues("insert_batch").Observe(time.Since(start).Seconds())

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("failed to commit transaction: %w", err)
	}
	zlog.Debug(ctx).
		Stringer("ref", ref).
		Int("inserted", len(es)).
		Msg("update_operation committed")
	return ref, nil
}

func hashEnrichment(r *driver.EnrichmentRecord) (k string, d []byte) {
	h := md5.New()
	sort.Strings(r.Tags)
	for _, t := range r.Tags {
		io.WriteString(h, t)
		h.Write([]byte("\x00"))
	}
	h.Write(r.Enrichment)
	return "md5", h.Sum(nil)
}

func (s *Store) GetEnrichment(ctx context.Context, name string, tags []string) ([]driver.EnrichmentRecord, error) {
	const query = `
WITH
	latest
		AS (
			SELECT
				max(id) AS id
			FROM
				update_operation
			WHERE
				updater = $1
		)
SELECT
	e.tags, e.data
FROM
	enrichment AS e,
	uo_enrich AS uo,
	latest
WHERE
	uo.uo = latest.id
	AND uo.enrich = e.id
	AND e.tags && $2::text[];`

	ctx = baggage.ContextWithValues(ctx,
		label.String("component", "internal/vulnstore/postgres/GetEnrichment"))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	results := make([]driver.EnrichmentRecord, 0, 8) // Guess at capacity.
	rows, err := s.pool.Query(ctx, query, name, tags)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		results = append(results, driver.EnrichmentRecord{})
		r := &results[i]
		if err := rows.Scan(&r.Tags, &r.Enrichment); err != nil {
			return nil, err
		}
		i++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}
