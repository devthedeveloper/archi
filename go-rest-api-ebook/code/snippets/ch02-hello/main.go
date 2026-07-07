package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	http.HandleFunc("GET /hello", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Hello from Go! 🎉")
	})

	log.Println("listening on http://localhost:4000 ...")
	log.Fatal(http.ListenAndServe(":4000", nil))
}
