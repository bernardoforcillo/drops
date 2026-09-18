package r2_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/cloudflare/d1"
	"github.com/bernardoforcillo/drops/cloudflare/r2"
)

func ExampleNew() {
	cf, err := cloudflare.New("your-account-id", cloudflare.WithAPIToken("your-api-token"))
	if err != nil {
		log.Fatal(err)
	}
	backups := r2.New(cf).Bucket("backups")
	_ = backups
}

// The archive a D1 database actually needs.
//
// Time Travel reaches back thirty days and dies with the database, so
// anything that has to outlive either — a compliance retention
// period, a restore into a different account — has to be a dump
// somewhere else. This is that, in the shape that does not hold the
// dump in memory: D1 writes it to a file, R2 streams the file up.
func Example_archiveAD1Database() {
	cf, err := cloudflare.New("your-account-id", cloudflare.WithAPIToken("your-api-token"))
	if err != nil {
		log.Fatal(err)
	}
	if err := archiveD1(context.Background(), cf, "your-database-id", "backups"); err != nil {
		log.Fatal(err)
	}
}

// archiveD1 exports a D1 database to a temporary file and streams the
// file into R2, so the dump never exists in this process's heap — a
// 300 MB one would otherwise be 300 MB of garbage to collect.
func archiveD1(ctx context.Context, cf *cloudflare.Client, databaseID, bucket string) error {
	admin := d1.NewAdmin(cf)
	backups := r2.New(cf).Bucket(bucket)

	dir, err := os.MkdirTemp("", "d1-export")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "dump.sql")
	exp, err := dumpTo(ctx, admin, databaseID, path)
	if err != nil {
		return err
	}

	// InfrequentAccess is the class for this: cheap to keep, charged
	// to read, and a backup is read approximately never. The key
	// carries the Time Travel bookmark the dump was taken at, so the
	// exact state it captured can still be named later.
	key := fmt.Sprintf("d1/%s/%s.sql", time.Now().UTC().Format("2006-01-02"), exp.Bookmark)
	_, err = backups.PutFile(ctx, key, path, r2.PutOptions{
		ContentType:  "application/sql",
		StorageClass: r2.InfrequentAccess,
	})
	return err
}

// dumpTo writes the export to path and closes the file before it is
// read back, which is the part a one-liner gets wrong.
func dumpTo(ctx context.Context, admin *d1.Admin, databaseID, path string) (*d1.Export, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	exp, err := admin.ExportTo(ctx, databaseID, f, d1.ExportOptions{})
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	return exp, nil
}

// A prefix and a delimiter make a listing read like a directory,
// which is what a retention sweep wants.
func ExampleBucket_List() {
	var backups *r2.Bucket
	ctx := context.Background()

	objs, err := backups.List(ctx, r2.ListOptions{Prefix: "d1/2026-08-", Delimiter: "/"})
	if err != nil {
		log.Fatal(err)
	}
	cutoff := time.Now().AddDate(0, -6, 0)
	for _, o := range objs {
		if o.LastModified.Before(cutoff) {
			if err := backups.Delete(ctx, o.Key); err != nil {
				log.Printf("could not delete %s: %v", o.Key, err)
			}
		}
	}
}

// A missing object is [cloudflare.ErrNotFound], the same sentinel
// every other Cloudflare backend answers with.
func ExampleBucket_Get() {
	var backups *r2.Bucket
	ctx := context.Background()

	data, err := backups.GetBytes(ctx, "d1/2026-09-15/dump.sql")
	switch {
	case errors.Is(err, cloudflare.ErrNotFound):
		fmt.Println("no backup for that day")
	case err != nil:
		log.Fatal(err)
	default:
		fmt.Printf("%d bytes of SQL\n", len(data))
	}
}
