// SPDX-License-Identifier: Unlicense OR MIT

package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

func main() {
	http.HandleFunc("/", vanityHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", port), nil))
}

// vanityHandler serves git location meta headers for the go tool.
func vanityHandler(w http.ResponseWriter, r *http.Request) {
	if www := "www."; strings.HasPrefix(r.URL.Host, www) {
		r.URL.Host = r.URL.Host[len(www):]
		http.Redirect(w, r, r.URL.String(), http.StatusMovedPermanently)
		return
	}
	if r.URL.Query().Get("go-get") == "1" {
		fmt.Fprintf(w, `<html><head>
<meta name="go-import" content="scatter.im git https://git.sr.ht/~eliasnaur/scatter">
<meta name="go-source" content="scatter.im https://git.sr.ht/~eliasnaur/scatter https://git.sr.ht/~eliasnaur/scatter/tree/master{/dir} https://git.sr.ht/~eliasnaur/scatter/tree/master{/dir}/{file}#L{line}">
</head></html>`)
		return
	}
	switch r.URL.Path {
	case "/":
		http.Redirect(w, r, "https://git.sr.ht/~eliasnaur/scatter", http.StatusFound)
	default:
		http.NotFound(w, r)
	}
}
