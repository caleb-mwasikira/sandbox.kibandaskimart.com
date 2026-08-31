package main

import (
	"database/sql"
	"log"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func initDB() *sql.DB {
	dbPath := filepath.Join(BaseStorageDir, DBFileName)
	_ = os.MkdirAll(BaseStorageDir, 0755)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("[-] Failed to open sqlite database: %v", err)
	}

	query := `CREATE TABLE IF NOT EXISTS os_images (
		name TEXT PRIMARY KEY,
		path TEXT NOT NULL
	);`
	if _, err := db.Exec(query); err != nil {
		log.Fatalf("[-] Failed to create table: %v", err)
	}

	return db
}
