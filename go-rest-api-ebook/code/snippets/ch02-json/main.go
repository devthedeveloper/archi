package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// The struct tags (the bits in backticks) control the JSON key names:
// Title becomes "title", CreatedAt becomes "created_at".
type Post struct {
	ID        int       `json:"id"`
	Title     string    `json:"title"`
	Score     int       `json:"score"`
	CreatedAt time.Time `json:"created_at"`
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /posts/{id}", func(w http.ResponseWriter, r *http.Request) {
		post := Post{
			ID:        1,
			Title:     "Go is pretty nice actually",
			Score:     42,
			CreatedAt: time.Now().UTC(),
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(post); err != nil {
			log.Println("encoding failed:", err)
		}
	})

	log.Println("listening on http://localhost:4000 ...")
	log.Fatal(http.ListenAndServe(":4000", mux))
}
