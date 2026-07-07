package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"gopherit/internal/store"
)

// envelope wraps every response, so JSON always has a named top-level
// key: {"post": {...}} instead of a bare {...}.
type envelope map[string]any

// writeJSON sends data as pretty-printed JSON with the given status code.
func (app *application) writeJSON(w http.ResponseWriter, status int, data envelope) {
	js, err := json.MarshalIndent(data, "", "\t")
	if err != nil {
		app.logger.Error("writeJSON failed", "error", err)
		http.Error(w, `{"error":"the server encountered a problem"}`, http.StatusInternalServerError)
		return
	}
	js = append(js, '\n')

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(js)
}

// readJSON decodes the request body into dst — strictly. It rejects
// bodies over 1 MB, unknown fields, trailing garbage and empty bodies,
// and turns the decoder's cryptic errors into human-friendly messages.
func (app *application) readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1_048_576) // 1 MB cap

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		var maxBytesErr *http.MaxBytesError

		switch {
		case errors.As(err, &syntaxErr):
			return fmt.Errorf("body contains badly-formed JSON (at character %d)", syntaxErr.Offset)
		case errors.Is(err, io.ErrUnexpectedEOF):
			return errors.New("body contains badly-formed JSON")
		case errors.As(err, &typeErr):
			if typeErr.Field != "" {
				return fmt.Errorf("body contains incorrect JSON type for field %q", typeErr.Field)
			}
			return fmt.Errorf("body contains incorrect JSON type (at character %d)", typeErr.Offset)
		case errors.Is(err, io.EOF):
			return errors.New("body must not be empty")
		case errors.As(err, &maxBytesErr):
			return fmt.Errorf("body must not be larger than %d bytes", maxBytesErr.Limit)
		default:
			return err
		}
	}

	// A second Decode should hit EOF. If not, there was extra JSON.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("body must only contain a single JSON value")
	}
	return nil
}

// readIDParam pulls the {id} path parameter out of the URL.
func (app *application) readIDParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, errors.New("invalid id parameter")
	}
	return id, nil
}

// problems collects validation failures: field name -> what's wrong.
type problems map[string]string

// check records a problem when ok is false. The first message for a
// field wins, so order your checks from most to least important.
func (p problems) check(ok bool, field, message string) {
	if !ok {
		if _, exists := p[field]; !exists {
			p[field] = message
		}
	}
}

// readPostFilters parses ?sort=&page=&page_size= with sane defaults.
func (app *application) readPostFilters(r *http.Request) (store.PostFilters, error) {
	q := r.URL.Query()
	f := store.PostFilters{Sort: "hot", Page: 1, PageSize: 20}

	if s := q.Get("sort"); s != "" {
		if s != "new" && s != "top" && s != "hot" {
			return f, errors.New("sort must be one of: new, top, hot")
		}
		f.Sort = s
	}
	if pg := q.Get("page"); pg != "" {
		n, err := strconv.Atoi(pg)
		if err != nil || n < 1 {
			return f, errors.New("page must be a positive whole number")
		}
		f.Page = n
	}
	if ps := q.Get("page_size"); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n < 1 || n > 100 {
			return f, errors.New("page_size must be between 1 and 100")
		}
		f.PageSize = n
	}
	return f, nil
}
