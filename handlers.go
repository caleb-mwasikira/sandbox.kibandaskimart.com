package main

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type UserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email"`
}

func AddUserHandler(w http.ResponseWriter, r *http.Request) {
	var req UserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Username == "" || req.Password == "" {
		http.Error(w, "username and password are required", http.StatusBadRequest)
		return
	}
	err := store.AddUser(User{Username: req.Username, Password: req.Password, Email: req.Email})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"status": "success", "username": req.Username, "container": req.Username + "-sandbox"})
}

func UpdateUserHandler(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	var req UserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err := store.UpdateUser(username, req.Email, req.Password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "updated", "username": username})
}

func DeleteUserHandler(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	err := store.DeleteUser(username)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "username": username})
}
