package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/cos"
	"everythingabout.me/backend/internal/storagebaseline"
	"everythingabout.me/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

var baselineOwner = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type baselineArtifact struct {
	Snapshot storagebaseline.Snapshot `json:"snapshot"`
	Report   storagebaseline.Report   `json:"report"`
	// Root identity excludes credentials. Back this mapping up independently.
	Connection map[string]string `json:"connection"`
}

// runStorageBaseline is read-only against Postgres/COS. Output is a new private
// file, never an overwrite. Offline recheck does not load application configuration.
func runStorageBaseline(args []string) error {
	flags := flag.NewFlagSet("storage-baseline", flag.ContinueOnError)
	owner := flags.String("owner-id", "", "explicit owner UUID for live capture")
	output := flags.String("output", "", "new private JSON artifact path")
	input := flags.String("input", "", "recheck a previously captured JSON artifact offline")
	quiesced := flags.Bool("writers-quiesced", false, "assert independently established native writer quiescence during live capture")
	quota := flags.Int64("quota-bytes", 0, "logical quota for offline comparison; zero means unlimited")
	timeout := flags.Duration("timeout", 10*time.Minute, "maximum live capture duration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *output == "" || *quota < 0 || *timeout <= 0 {
		return fmt.Errorf("storage-baseline requires --output and valid nonnegative quota/positive timeout")
	}
	var artifact baselineArtifact
	if *input != "" {
		if *owner != "" || *quiesced {
			return fmt.Errorf("offline recheck cannot override owner or writer-quiescence evidence")
		}
		file, err := os.Open(*input)
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(file)
		err = decoder.Decode(&artifact)
		if err == nil {
			var extra any
			if end := decoder.Decode(&extra); end != io.EOF {
				err = fmt.Errorf("baseline input must contain exactly one JSON artifact")
			}
		}
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		artifact.Report = storagebaseline.Compare(artifact.Snapshot, *quota)
	} else {
		if !baselineOwner.MatchString(*owner) {
			return fmt.Errorf("live capture requires --owner-id as a lowercase UUID")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if err = cfg.ValidateCOS(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		artifact, err = captureStorageBaseline(ctx, cfg, *owner, *quiesced)
		if err != nil {
			return err
		}
		artifact.Report = storagebaseline.Compare(artifact.Snapshot, cfg.QuotaBytes)
	}
	if err := writeBaselineArtifact(*output, artifact); err != nil {
		return err
	}
	fmt.Printf("baseline saved: native files=%d, native bytes=%d, index bytes=%d, issues=%d, reservation baseline ready=%t\n", artifact.Report.NativeFiles, artifact.Report.NativeBytes, artifact.Report.IndexBytes, len(artifact.Report.Issues), artifact.Report.ReservationBaselineReady)
	return nil
}

func writeBaselineArtifact(path string, artifact baselineArtifact) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(artifact)
	syncErr := file.Sync()
	closeErr := file.Close()
	// Keep a partial artifact on failure as diagnostic evidence; never overwrite it.
	return errors.Join(encodeErr, syncErr, closeErr)
}

func captureStorageBaseline(ctx context.Context, cfg *config.Config, owner string, quiesced bool) (baselineArtifact, error) {
	a := baselineArtifact{Snapshot: storagebaseline.Snapshot{Version: 1, OwnerID: owner, Backend: "cos", ContentRoot: "drive/" + owner + "/", StartedAt: time.Now().UTC(), WritersQuiesced: quiesced, Index: []storagebaseline.IndexEntry{}, LegacyUploads: []json.RawMessage{}}, Connection: map[string]string{"backend": "cos", "bucket": cfg.COS.Bucket, "region": cfg.COS.Region, "contentRoot": "drive/" + owner + "/"}}
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return a, err
	}
	defer db.Close()
	tx, err := db.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return a, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var found bool
	if err = tx.QueryRow(ctx, `select exists(select 1 from users where id=$1::uuid)`, owner).Scan(&found); err != nil {
		return a, err
	}
	if !found {
		return a, fmt.Errorf("baseline owner not found")
	}
	rows, err := tx.Query(ctx, `select id::text,key,path,is_dir,size,etag,to_jsonb(entry) from file_entries entry where owner_id=$1::uuid order by key`, owner)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		var e storagebaseline.IndexEntry
		if err = rows.Scan(&e.ID, &e.Key, &e.Path, &e.Folder, &e.Size, &e.ETag, &e.Metadata); err != nil {
			rows.Close()
			return a, err
		}
		a.Snapshot.Index = append(a.Snapshot.Index, e)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return a, err
	}
	// LegacyUploads stays in the artifact shape, always empty: the legacy upload
	// sessions table was retired (sql/053), but artifacts written before that still
	// carry rows, and the offline --input recheck must keep reading them.
	if err = tx.Commit(ctx); err != nil {
		return a, err
	}
	a.Snapshot.IndexComplete = true
	client, err := cos.New(cfg.COS)
	if err != nil {
		return a, err
	}
	a.Snapshot.Native, err = client.CaptureBaseline(ctx, a.Snapshot.ContentRoot)
	if err != nil {
		return a, err
	}
	a.Snapshot.NativeComplete = true
	a.Snapshot.FinishedAt = time.Now().UTC()
	return a, nil
}
