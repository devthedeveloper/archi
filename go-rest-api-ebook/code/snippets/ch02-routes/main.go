package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	mux := http.NewServeMux()

	// Method + exact path.
	mux.HandleFunc("GET /posts", listPosts)
	mux.HandleFunc("POST /posts", createPost)

	// {id} is a path parameter — it matches one segment.
	mux.HandleFunc("GET /posts/{id}", getPost)

	log.Println("listening on http://localhost:4000 ...")
	log.Fatal(http.ListenAndServe(":4000", mux))
}

func listPosts(w http.ResponseWriter, r *http.Request) {
	// Query parameters live in the URL after the "?".
	sort := r.URL.Query().Get("sort") // "" when absent
	if sort == "" {
		sort = "hot"
	}
	fmt.Fprintf(w, "a list of posts, sorted by %q\n", sort)
}

func createPost(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusCreated) // 201
	fmt.Fprintln(w, "pretend we created a post")
}

func getPost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id") // whatever matched {id}
	fmt.Fprintf(w, "you asked for post number %s\n", id)
}
