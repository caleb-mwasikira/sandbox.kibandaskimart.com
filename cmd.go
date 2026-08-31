package main

import (
	"database/sql"
	"fmt"
	"log"
	"path/filepath"
)

func addOSImage(name, imagePath string) {
	absPath, err := filepath.Abs(imagePath)
	if err != nil {
		log.Fatalf("[-] Invalid path: %v", err)
	}

	_, err = db.Exec("INSERT OR REPLACE INTO os_images (name, path) VALUES (?, ?)", name, absPath)
	if err != nil {
		log.Fatalf("[-] Failed to add OS/ISO image in database: %v", err)
	}

	fmt.Printf("[+] Successfully registered image '%s' -> %s\n", name, absPath)
}

func deleteOSImage(name string) {
	result, err := db.Exec("DELETE FROM os_images WHERE name = ?", name)
	if err != nil {
		log.Fatalf("[-] Failed to delete OS image from database: %v", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Fatalf("[-] Failed to check deletion result: %v", err)
	}

	if rowsAffected == 0 {
		fmt.Printf("[-] OS image '%s' not found in database.\n", name)
		return
	}

	fmt.Printf("[+] Successfully deleted OS image '%s' from database.\n", name)
}

func listOSImages() {
	rows, err := db.Query("SELECT name, path FROM os_images")
	if err != nil {
		log.Fatalf("[-] Failed to query database: %v", err)
	}
	defer rows.Close()

	fmt.Println("Available OS/ISO Images:")
	fmt.Println("----------------------------------------")
	found := false
	for rows.Next() {
		var name, path string
		if err := rows.Scan(&name, &path); err != nil {
			continue
		}
		fmt.Printf("Name: %s\nPath: %s\n\n", name, path)
		found = true
	}

	if err := rows.Err(); err != nil {
		log.Printf("[-] Error during rows iteration: %v", err)
	}

	if !found {
		fmt.Println("No OS images registered yet.")
	}
}

type OSImageRecord struct {
	Name string
	Path string
}

func getOSImage(db *sql.DB, name string) (OSImageRecord, error) {
	var record OSImageRecord
	row := db.QueryRow("SELECT name, path FROM os_images WHERE name = ?", name)
	err := row.Scan(&record.Name, &record.Path)
	if err != nil {
		return record, err
	}
	return record, nil
}
