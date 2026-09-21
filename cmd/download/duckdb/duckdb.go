package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	_ "github.com/duckdb/duckdb-go/v2"
)

// duckdbExtensions is the list of DuckDB extensions required by WeKnora's
// data analysis tool. `spatial` is used for layer metadata (st_read_meta)
// so we can enumerate sheet names from Excel files, while `excel` provides
// the dedicated read_xlsx reader with proper type inference.
var duckdbExtensions = []string{"spatial", "excel"}

// localExtensionRepo returns the directory to install extensions from when
// DUCKDB_EXTENSION_DIR is set, or "" to always use the online registry.
//
// Build environments with unreliable access to extensions.duckdb.org pre-seed
// a local mirror with the layout <dir>/<duckdb-version>/<platform>/<ext>.duckdb_extension
// (uncompressed; see .build-cache/duckdb). Installation tries the local repo
// first and falls back to the online INSTALL on any failure, so unset or
// incomplete caches keep the default behaviour.
func localExtensionRepo() string {
	return os.Getenv("DUCKDB_EXTENSION_DIR")
}

func downloadExtensions() {
	ctx := context.Background()

	sqlDB, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		panic(err)
	}
	defer sqlDB.Close()

	for _, ext := range duckdbExtensions {
		if repo := localExtensionRepo(); repo != "" {
			if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf("INSTALL %s FROM '%s';", ext, repo)); err == nil {
				loadExtension(ctx, sqlDB, ext)
				continue
			}
			// Local mirror miss (wrong version layout, missing file): fall
			// through to the online registry below.
		}
		if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf("INSTALL %s;", ext)); err != nil {
			panic(fmt.Errorf("failed to install %s extension: %w", ext, err))
		}
		loadExtension(ctx, sqlDB, ext)
	}
}

func loadExtension(ctx context.Context, sqlDB *sql.DB, ext string) {
	if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf("LOAD %s;", ext)); err != nil {
		panic(fmt.Errorf("failed to load %s extension: %w", ext, err))
	}
}

func main() {
	downloadExtensions()
}
