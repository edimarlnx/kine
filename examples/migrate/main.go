// migrate is a CLI tool that copies all key-value pairs from one kine (or etcd)
// endpoint to another. It handles binary values correctly and paginates through
// large datasets without loading everything into memory at once.
//
// Usage:
//
//	go run ./examples/migrate \
//	  -source http://localhost:2379 \
//	  -target http://localhost:2380
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func main() {
	source := flag.String("source", "http://localhost:2379", "Source kine/etcd endpoint (e.g. PostgreSQL kine)")
	target := flag.String("target", "http://localhost:2380", "Target kine/etcd endpoint (e.g. MongoDB kine)")
	batch := flag.Int64("batch", 500, "Number of keys to fetch per batch")
	dialTimeout := flag.Duration("dial-timeout", 10*time.Second, "Timeout for connecting to endpoints")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srcClient, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{*source},
		DialTimeout: *dialTimeout,
	})
	if err != nil {
		log.Fatalf("connecting to source %s: %v", *source, err)
	}
	defer srcClient.Close()

	dstClient, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{*target},
		DialTimeout: *dialTimeout,
	})
	if err != nil {
		log.Fatalf("connecting to target %s: %v", *target, err)
	}
	defer dstClient.Close()

	fmt.Printf("source: %s\n", *source)
	fmt.Printf("target: %s\n", *target)
	fmt.Println("starting migration...")

	total, err := migrate(ctx, srcClient, dstClient, *batch)
	if err != nil {
		fmt.Println()
		log.Fatalf("migration failed after %d keys: %v", total, err)
	}

	fmt.Printf("\nmigration complete: %d keys restored\n", total)
}

// allKeysRangeEnd is the range end passed to kine to list all Kubernetes keys.
//
// kine's list.go decodes the rangeEnd by decrementing its last byte to obtain
// the prefix: prefix = rangeEnd[:n-1] + (rangeEnd[n-1] - 1).
// Sending "0" (ASCII 0x30) yields prefix "/" (0x30 - 1 = 0x2F), which covers
// all Kubernetes keys stored under /registry/... .
//
// Using clientv3.WithFromKey() instead would cause kine to compute
// '\x00' - 1 = '\xff' (byte underflow), which PostgreSQL rejects as invalid UTF-8.
const allKeysRangeEnd = "0"

// migrate reads all keys from src in pages of batchSize and writes them to dst.
// Returns the total number of keys successfully written.
func migrate(ctx context.Context, src, dst *clientv3.Client, batchSize int64) (int, error) {
	fromKey := "/"
	total := 0

	for {
		resp, err := src.Get(ctx, fromKey,
			clientv3.WithRange(allKeysRangeEnd),
			clientv3.WithLimit(batchSize),
		)
		if err != nil {
			return total, fmt.Errorf("fetching batch at %q: %w", fromKey, err)
		}

		for _, kv := range resp.Kvs {
			// Go strings preserve all bytes including null bytes, so binary
			// protobuf values are passed through clientv3.Put correctly.
			if _, err := dst.Put(ctx, string(kv.Key), string(kv.Value)); err != nil {
				return total, fmt.Errorf("putting key %q: %w", kv.Key, err)
			}
			total++
			fmt.Printf("\r  %d keys migrated", total)
		}

		if !resp.More {
			break
		}

		// Advance the cursor past the last returned key.
		// Appending \x01 produces the smallest UTF-8-valid string greater than
		// lastKey. All kine key paths are printable ASCII (bytes >= 0x2F), so no
		// real key exists between lastKey and lastKey+\x01.
		lastKey := resp.Kvs[len(resp.Kvs)-1].Key
		fromKey = string(lastKey) + "\x01"
	}

	return total, nil
}
