package main

import (
	"database/sql"
	"fmt"
	"log"

	_ "modernc.org/sqlite"
)

var db *sql.DB

func initDB() *sql.DB {
	var err error
	db, err = sql.Open("sqlite", "sandbox.db")
	if err != nil {
		log.Fatalf("[-] Failed to open database: %v", err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS containers (
		name TEXT PRIMARY KEY,
		os TEXT,
		status TEXT,
		password TEXT
	)`)
	if err != nil {
		log.Fatalf("[-] Failed to create containers table: %v", err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS users (
		username TEXT PRIMARY KEY,
		password TEXT,
		email TEXT
	)`)
	if err != nil {
		log.Fatalf("[-] Failed to create users table: %v", err)
	}

	return db
}

type User struct {
	Username string
	Password string
	Email    string
}

var store = &SQLiteStore{}

type SQLiteStore struct{}

func (s *SQLiteStore) AddUser(u User) error {
	_, err := db.Exec("INSERT OR REPLACE INTO users (username, password, email) VALUES (?, ?, ?)", u.Username, u.Password, u.Email)
	return err
}

func (s *SQLiteStore) UpdateUser(username, email, password string) error {
	if email != "" && password != "" {
		_, err := db.Exec("UPDATE users SET email = ?, password = ? WHERE username = ?", email, password, username)
		return err
	} else if email != "" {
		_, err := db.Exec("UPDATE users SET email = ? WHERE username = ?", email, username)
		return err
	} else if password != "" {
		_, err := db.Exec("UPDATE users SET password = ? WHERE username = ?", password, username)
		return err
	}
	return fmt.Errorf("no fields to update")
}

func (s *SQLiteStore) DeleteUser(username string) error {
	_, err := db.Exec("DELETE FROM users WHERE username = ?", username)
	return err
}

func (s *SQLiteStore) ValidateUser(username, password string) bool {
	var storedPassword string
	err := db.QueryRow("SELECT password FROM users WHERE username = ?", username).Scan(&storedPassword)
	if err != nil {
		return false
	}
	return storedPassword == password
}
